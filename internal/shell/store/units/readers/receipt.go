package readers

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// ReceiptLookup is the store-backed token.ReceiptLookup.
//
// It answers one scoped idempotency identity from the durable receipts table. A
// missing receipt is (Record{}, false, nil); an unreadable row is an error the
// caller must not paper over. The returned record is exactly the committed,
// secret-free receipt: the adapter never re-derives, re-encodes or enriches the
// result, so a credential receipt can never gain token plaintext on the way out.
type ReceiptLookup struct {
	scanner store.Scanner
}

// NewReceiptLookup builds the receipt lookup.
func NewReceiptLookup(scanner store.Scanner) (*ReceiptLookup, error) {
	if scanner == nil {
		return nil, fault.New(fault.InvalidInput, "receipt lookup requires a scanner")
	}
	return &ReceiptLookup{scanner: scanner}, nil
}

var _ token.ReceiptLookup = (*ReceiptLookup)(nil)

// Lookup returns the committed receipt for one scoped key, if any.
func (lookup *ReceiptLookup) Lookup(ctx context.Context, key receipt.Key) (receipt.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return receipt.Record{}, false, err
	}
	if err := key.Validate(); err != nil {
		return receipt.Record{}, false, err
	}
	scope := tenancy.Scope{TenantID: key.TenantID, ProjectID: key.ProjectID}
	if err := scope.Validate(); err != nil {
		return receipt.Record{}, false, err
	}
	page, err := lookup.scanner.Query(ctx, scope, store.ScanQuery{
		Table: "receipts",
		Equals: map[string]any{
			"principal_id": string(key.PrincipalID),
			"operation":    key.Operation,
			"caller_key":   key.CallerKey,
		},
		Limit: 1,
	})
	if err != nil {
		return receipt.Record{}, false, err
	}
	if len(page.Rows) == 0 {
		return receipt.Record{}, false, nil
	}
	record, err := receiptFromRow(page.Rows[0])
	if err != nil {
		return receipt.Record{}, false, err
	}
	if record.Key != key {
		return receipt.Record{}, false, fault.New(fault.InternalFailure, "durable receipt identity does not match its lookup key")
	}
	return record, true, nil
}

// receiptFromRow rebuilds one durable receipt from its stored columns.
func receiptFromRow(row store.Row) (receipt.Record, error) {
	identity, err := rowString(row, "id")
	if err != nil {
		return receipt.Record{}, err
	}
	tenant, err := rowString(row, "tenant_id")
	if err != nil {
		return receipt.Record{}, err
	}
	project, err := rowNullableString(row, "project_id")
	if err != nil {
		return receipt.Record{}, err
	}
	principal, err := rowString(row, "principal_id")
	if err != nil {
		return receipt.Record{}, err
	}
	operation, err := rowString(row, "operation")
	if err != nil {
		return receipt.Record{}, err
	}
	callerKey, err := rowString(row, "caller_key")
	if err != nil {
		return receipt.Record{}, err
	}
	hash, err := rowBytes(row, "content_sha256")
	if err != nil {
		return receipt.Record{}, err
	}
	fingerprint, err := contentHash(hash)
	if err != nil {
		return receipt.Record{}, err
	}
	result, err := rowBytes(row, "result")
	if err != nil {
		return receipt.Record{}, err
	}
	execution, err := rowNullableString(row, "execution_id")
	if err != nil {
		return receipt.Record{}, err
	}
	created, err := rowTime(row, "created_at")
	if err != nil {
		return receipt.Record{}, err
	}
	completed, err := rowNullableTime(row, "completed_at")
	if err != nil {
		return receipt.Record{}, err
	}
	if completed != nil && len(result) == 0 {
		return receipt.Record{}, fault.New(fault.InternalFailure, fmt.Sprintf("completed receipt %q has no result", identity))
	}
	record := receipt.Record{
		ID:          id.ReceiptID(identity),
		Key:         receipt.Key{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project), PrincipalID: id.PrincipalID(principal), Operation: operation, CallerKey: callerKey},
		ContentHash: fingerprint,
		Result:      result,
		ExecutionID: id.ExecutionID(execution),
		At:          created,
	}
	snapshot, err := rowBytes(row, "snapshot_sha256")
	if err != nil {
		return receipt.Record{}, err
	}
	if len(snapshot) > 0 {
		if len(snapshot) != 32 {
			return receipt.Record{}, fault.New(fault.InternalFailure, fmt.Sprintf("receipt %q has a malformed snapshot hash", identity))
		}
		var digest [32]byte
		copy(digest[:], snapshot)
		record.SnapshotHash = &digest
	}
	return record, nil
}
