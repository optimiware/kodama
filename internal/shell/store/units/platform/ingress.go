package platform

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
)

// IngressAdapter is the store-backed event.IngressPort.
//
// Ingest maps one complete custom-observation unit to one store.Unit: the
// observation envelope is the unit's event publication intent, and the receipt
// and audit rows commit in the same transaction. The current-authority reads the
// capability attached to the unit become read-only locks inside that
// transaction. The adapter writes no action or execution state, so a custom
// observation can never settle or advance a request.
type IngressAdapter struct {
	executor store.Executor
	scanner  store.Scanner
}

// NewIngressAdapter builds the ingress adapter.
func NewIngressAdapter(executor store.Executor, scanner store.Scanner) (*IngressAdapter, error) {
	if executor == nil {
		return nil, fault.New(fault.InvalidInput, "ingress adapter requires an executor")
	}
	return &IngressAdapter{executor: executor, scanner: scanner}, nil
}

var _ event.IngressPort = (*IngressAdapter)(nil)

// Ingest commits one custom observation and reports the committed identities.
func (adapter *IngressAdapter) Ingest(ctx context.Context, unit event.IngressUnit) (event.IngressResult, error) {
	if err := unit.Validate(); err != nil {
		return event.IngressResult{}, err
	}
	locks, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Owner, unit.Authority, authorityFacts{
		ExecutionID: unit.ExecutionID,
		ActorKind:   sourcePrincipalKind(unit.Event.Source.Kind),
	})
	if err != nil {
		return event.IngressResult{}, err
	}
	record := unit.Receipt
	result, err := adapter.executor.Execute(ctx, unit.Owner, store.Unit{
		Locks:   locks,
		Receipt: &record,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  []event.Envelope{unit.Event},
	})
	if err != nil {
		return event.IngressResult{}, err
	}
	if result.Replayed {
		return replayIngressResult(result.Receipt)
	}
	return event.IngressResult{
		EventID:   unit.Event.ID,
		ReceiptID: unit.Receipt.ID,
		Replayed:  false,
	}, nil
}

// sourcePrincipalKind maps the authenticated event source kind onto the
// principal kind whose authority facts the commit boundary re-reads.
func sourcePrincipalKind(kind event.SourceKind) identity.PrincipalKind {
	switch kind {
	case event.SourceExecution:
		return identity.KindExecution
	case event.SourceService:
		return identity.KindService
	case event.SourceShell:
		return identity.KindHuman
	default:
		return ""
	}
}

// replayIngressResult decodes the original committed identities from the
// replayed receipt. An ingress receipt's durable result body is the committed
// event id, so an identical retry converges on the same event.
func replayIngressResult(record receipt.Record) (event.IngressResult, error) {
	eventID, err := id.ParseEventID(string(record.Result))
	if err != nil {
		return event.IngressResult{}, fault.Wrap(fault.InternalFailure, "replayed ingress receipt has no valid event id", err)
	}
	if _, err := id.ParseReceiptID(string(record.ID)); err != nil {
		return event.IngressResult{}, fault.Wrap(fault.InternalFailure, "replayed ingress receipt has no valid receipt id", err)
	}
	return event.IngressResult{EventID: eventID, ReceiptID: record.ID, Replayed: true}, nil
}
