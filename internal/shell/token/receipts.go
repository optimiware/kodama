// Durable receipt replay for credential mutations.
//
// A credential commit stores its receipt, audit rows and event intent in the
// same transaction as the nonce/expiry change. The receipt is the durable
// result of that operation. A caller whose response was lost retries the same
// command with the same caller key and content; the first commit already
// advanced the principal/service version and (for issue/rotate) installed a
// current credential, so the ordinary state preflights would refuse the retry
// instead of returning the original result.
//
// ReceiptLookup is the consumer-owned read seam consulted before those
// state-dependent preflights. It returns the committed, secret-free receipt
// record. Authorization, current-principal confirmation and the disabled-service
// refusal still run first; the receipt is never a cached permission.
package token

import (
	"context"
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/receipt"
)

// ReceiptLookup reads the durable receipt for one scoped idempotency key. A
// missing receipt is (Record{}, false, nil), never an error; an unreadable
// receipt is a dependency failure the caller must not paper over.
type ReceiptLookup interface {
	Lookup(context.Context, receipt.Key) (receipt.Record, bool, error)
}

// replayedIssueResult decodes the durable issue/rotate result of an equal
// receipt. The receipt body never carries token plaintext or a nonce, so the
// replay returns the committed state/version with no token and no delivery.
func replayedIssueResult(record receipt.Record) (IssueResult, error) {
	var committed IssueResult
	if err := json.Unmarshal(record.Result, &committed); err != nil {
		return IssueResult{}, fault.Wrap(fault.InternalFailure, "replayed credential result could not be decoded", err)
	}
	if !committed.State.Valid() || committed.Version == 0 {
		return IssueResult{}, fault.New(fault.InternalFailure, "replayed credential result has no committed state")
	}
	committed.Replayed = true
	committed.Token = ""
	committed.Delivery = nil
	committed.Delivered = false
	return committed, nil
}

// replayedRevokeResult decodes the durable revocation result of an equal
// receipt. The invalidation is a post-commit effect and is re-run by the
// caller, not read back from the receipt.
func replayedRevokeResult(record receipt.Record) (RevokeResult, error) {
	var committed RevokeResult
	if err := json.Unmarshal(record.Result, &committed); err != nil {
		return RevokeResult{}, fault.Wrap(fault.InternalFailure, "replayed credential result could not be decoded", err)
	}
	if !committed.Mutation.State.Valid() || committed.Mutation.Version == 0 {
		return RevokeResult{}, fault.New(fault.InternalFailure, "replayed credential result has no committed state")
	}
	committed.Mutation.Replayed = true
	committed.Invalidated = false
	return committed, nil
}

// lookupReceipt consults the receipt seam for one request. A request without a
// receipt identity cannot replay and falls through to the ordinary path.
func (manager *Manager) lookupReceipt(ctx context.Context, requested receipt.Record) (receipt.Record, bool, error) {
	if manager.receipts == nil || requested.Key.CallerKey == "" {
		return receipt.Record{}, false, nil
	}
	record, found, err := manager.receipts.Lookup(ctx, requested.Key)
	if err != nil {
		return receipt.Record{}, false, publicFault("credential receipt is unavailable", err, fault.DependencyUnavailable)
	}
	if !found {
		return receipt.Record{}, false, nil
	}
	if record.ContentHash != requested.ContentHash {
		return receipt.Record{}, false, fault.New(fault.ChangedIdempotencyContent,
			"a receipt with the same caller key already holds different request content")
	}
	return record, true, nil
}

// replayIssue returns the committed issue/rotate result when an equal receipt
// already exists, before any credential-presence or expected-version preflight.
func (manager *Manager) replayIssue(ctx context.Context, request IssueRequest) (IssueResult, bool, error) {
	record, found, err := manager.lookupReceipt(ctx, request.Receipt)
	if err != nil || !found {
		return IssueResult{}, false, err
	}
	committed, err := replayedIssueResult(record)
	if err != nil {
		return IssueResult{}, false, err
	}
	return committed, true, nil
}

// replayRevoke returns the committed revocation result and re-runs the
// post-commit invalidation without mutating again. A failing invalidation on
// the replay is still retryable; a later retry can succeed.
func (manager *Manager) replayRevoke(ctx context.Context, request RevokeRequest, now time.Time) (RevokeResult, bool, error) {
	record, found, err := manager.lookupReceipt(ctx, request.Receipt)
	if err != nil || !found {
		return RevokeResult{}, false, err
	}
	committed, err := replayedRevokeResult(record)
	if err != nil {
		return RevokeResult{}, false, err
	}
	invalidation := Invalidation{
		Scope:       request.Scope,
		PrincipalID: request.PrincipalID,
		ExecutionID: request.ExecutionID,
		ServiceID:   request.ServiceID,
		Reason:      request.Reason,
		At:          now,
	}
	if err := invalidation.Validate(); err != nil {
		return RevokeResult{Mutation: committed.Mutation, Invalidation: invalidation}, true, err
	}
	committed.Invalidation = invalidation
	if err := manager.invalidations.Invalidate(ctx, invalidation); err != nil {
		return committed, true, publicFault("credential invalidation failed", err, fault.UncertainExternalEffect)
	}
	committed.Invalidated = true
	return committed, true, nil
}
