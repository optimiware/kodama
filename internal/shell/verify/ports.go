package verify

import (
	"context"

	"github.com/optimiware/kodama/internal/shell/resource"
)

// ReportPort applies one complete verified-report commit unit.
//
// Report must recheck the reporting service, binding and resource ownership
// inside the same transaction as the deduplication lookup, receipt, audit and
// event publication intent. Actor/execution causation is taken from the
// referenced verification record, never from the report body.
type ReportPort interface {
	Report(context.Context, ReportUnit) (ReportResult, error)
}

// RecordPort creates one complete durable verification record.
//
// The verifier uses this port instead of a private store substitute. The
// adapter must re-read the current service, binding and actor facts supplied as
// authority reads inside the same transaction as the record, receipt, audit and
// event publication intent.
type RecordPort interface {
	Record(context.Context, RecordUnit) (RecordResult, error)
}

// BindingReader reads the current binding that authorizes one calling service
// on one resource. It is current evidence for one verification, never durable
// permission.
type BindingReader interface {
	ReadBinding(context.Context, BindingQuery) (resource.Binding, error)
}

// EvidenceReader reads prior succeeded request facts in one execution so the
// verifier can perform the exact typed approval match. It returns only durable
// facts.
type EvidenceReader interface {
	ReadEvidence(context.Context, EvidenceQuery) ([]Evidence, error)
}
