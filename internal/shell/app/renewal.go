package app

// Workload credential renewal in the Shell composition.
//
// A container execution credential is issued at activation with a TTL bounded
// by the eight-hour maximum and the persisted action deadline (KODAMA.md
// "Execution lifetime", S05 "Container activation"). A long-running action
// therefore needs a renewal loop: while the execution is running, holds a live
// committed credential inside the renewal margin and its current action
// deadline is still in the future, the composition atomically replaces the
// committed nonce/expiry through token.Manager and delivers the replacement
// through the runtime credential-refresh seam. The workload is never restarted
// or re-prepared.
//
// Every fact is reconstructed from durable state on each pass (running
// executions, their committed credential pair and the current action deadline),
// so a restarted composition resumes renewal without any in-memory state. The
// loop is not new dispatch: it never consults the installation dispatch gate,
// because a fenced restore must keep a running workload's credential current.
//
// A failed delivery is retried with the same committed token. The pending
// delivery is kept only for the retry; the durable committed pair is what makes
// the token current, and the token manager's receipt makes a repeated rotate of
// the same generation idempotent, so a delivery retry never mints a second
// token. Token and secret material never appears in a log, event, receipt or
// error.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

const (
	// DefaultRenewBefore is the composition renewal margin when Config leaves
	// it unset. A credential is renewed once its committed expiry is at or
	// before now+margin.
	DefaultRenewBefore = 10 * time.Minute
	// DefaultRenewInterval is the composition renewal scan cadence when Config
	// leaves it unset. It is deliberately longer than the general loop cadence:
	// renewal reads durable state and mints only near expiry.
	DefaultRenewInterval = time.Minute
	// renewalScanLimit bounds one cross-scope execution page. The loop follows
	// the returned cursor until the durable scan is exhausted.
	renewalScanLimit = 128
	// renewalCredentialOperation is the receipt operation identity of one
	// workload credential renewal. It is not a public registry operation.
	renewalCredentialOperation = "execution.credential.renew"
)

// renewalLoop owns the workload credential renewal scan. It is driven by one
// composition goroutine; pending deliveries are only retried by that same
// goroutine.
type renewalLoop struct {
	controller  store.Controller
	credentials token.CredentialReader
	manager     *token.Manager
	deliverer   token.Deliverer
	clock       Clock
	ids         *bridges.IDs
	renewBefore time.Duration
	interval    time.Duration
	logger      *slog.Logger

	// pending holds a committed-but-undelivered renewal token per execution. It
	// is a retry hint, never the source of renewal eligibility: the durable
	// committed pair and deadline are re-read every pass.
	pending map[id.ExecutionID]pendingRenewal
}

// pendingRenewal is one committed renewal token awaiting delivery.
type pendingRenewal struct {
	scope       tenancy.Scope
	principalID id.PrincipalID
	executionID id.ExecutionID
	nonce       []byte
	delivery    token.Delivery
}

// renewalLoopConfig binds every renewal dependency.
type renewalLoopConfig struct {
	Controller  store.Controller
	Credentials token.CredentialReader
	Manager     *token.Manager
	Deliverer   token.Deliverer
	Clock       Clock
	IDs         *bridges.IDs
	RenewBefore time.Duration
	Interval    time.Duration
	Logger      *slog.Logger
}

// newRenewalLoop refuses a partially wired renewal loop so the composition can
// never silently skip durable reads, minting or delivery.
func newRenewalLoop(config renewalLoopConfig) (*renewalLoop, error) {
	switch {
	case config.Controller == nil:
		return nil, fault.New(fault.InvalidInput, "renewal loop requires a controller reader")
	case config.Credentials == nil:
		return nil, fault.New(fault.InvalidInput, "renewal loop requires a credential reader")
	case config.Manager == nil:
		return nil, fault.New(fault.InvalidInput, "renewal loop requires a credential manager")
	case config.Deliverer == nil:
		return nil, fault.New(fault.InvalidInput, "renewal loop requires a credential deliverer")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "renewal loop requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InvalidInput, "renewal loop requires an identity source")
	case config.RenewBefore <= 0:
		return nil, fault.New(fault.InvalidInput, "renewal loop requires a positive renewal margin")
	case config.Interval <= 0:
		return nil, fault.New(fault.InvalidInput, "renewal loop requires a positive scan interval")
	}
	return &renewalLoop{
		controller:  config.Controller,
		credentials: config.Credentials,
		manager:     config.Manager,
		deliverer:   config.Deliverer,
		clock:       config.Clock,
		ids:         config.IDs,
		renewBefore: config.RenewBefore,
		interval:    config.Interval,
		logger:      config.Logger,
		pending:     make(map[id.ExecutionID]pendingRenewal),
	}, nil
}

// run scans durable state on the configured cadence until cancellation. A scan
// failure is logged by fault class and retried on the next pass; it never stops
// the loop or another execution's renewal.
func (loop *renewalLoop) run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := loop.scan(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			loop.logFault("", err)
		}
		if err := loop.clock.Sleep(ctx, loop.interval); err != nil {
			return err
		}
	}
}

// scan retries pending deliveries, then walks every durable running execution.
// The controller scan is cross-scope; every renewal still uses only that
// execution's own tenant/project scope and tenant derivation key.
func (loop *renewalLoop) scan(ctx context.Context) error {
	now := loop.clock.Now()
	if now.IsZero() {
		return fault.New(fault.InternalFailure, "renewal clock returned zero time")
	}
	loop.retryPending(ctx, now)
	if err := ctx.Err(); err != nil {
		return err
	}
	cursor := ""
	for {
		page, err := loop.controller.Scan(ctx, store.ControllerScan{
			Table:   "executions",
			Equals:  map[string]any{"state": "running"},
			OrderBy: []store.Order{{Column: "id"}},
			After:   cursor,
			Limit:   renewalScanLimit})
		if err != nil {
			return err
		}
		for _, row := range page.Rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			loop.renewExecution(ctx, row, now)
		}
		if !page.HasMore || page.Next == "" {
			return nil
		}
		cursor = page.Next
	}
}

// renewExecution decides one durable running execution. It renews only a live
// committed credential inside the margin whose current action deadline is still
// in the future and can grant at least the minimum lifetime.
func (loop *renewalLoop) renewExecution(ctx context.Context, row store.Row, now time.Time) {
	executionIDText, err := rowText(row, "id")
	if err != nil {
		loop.logFault("", err)
		return
	}
	executionID := id.ExecutionID(executionIDText)
	if _, pending := loop.pending[executionID]; pending {
		return
	}
	scope, principalID, version, credential, err := renewalExecutionFact(row)
	if err != nil {
		loop.logFault(executionID, err)
		return
	}
	if !credential.Present() {
		// A request wait or a cleared/revoked execution has no live credential.
		return
	}
	if now.Add(loop.renewBefore).Before(credential.Expiry) {
		// Outside the renewal margin.
		return
	}
	deadline, ok, err := loop.actionDeadline(ctx, executionID)
	if err != nil {
		loop.logFault(executionID, err)
		return
	}
	if !ok || !now.Before(deadline) {
		// No current action deadline, or the action is already past it: never
		// extend a credential past the action deadline.
		return
	}
	if deadline.Sub(now) < token.DefaultBounds.MinTTL {
		// The capped lifetime is below the minimum, so no token may be
		// minted and the committed credential stays untouched.
		return
	}
	// A rotation must extend the committed authority. A credential already
	// capped at its action deadline would rotate D to D: a new nonce and
	// delivery for the same expiry. That is pointless and, worse, it commits a
	// receipt for generation D that a later action with a later deadline would
	// replay instead of renewing. Leave the pair untouched so the extension is
	// still possible.
	lifetime := now.Add(token.DefaultBounds.MaxTTL)
	if deadline.Before(lifetime) {
		lifetime = deadline
	}
	if !lifetime.After(credential.Expiry) {
		return
	}
	loop.renew(ctx, scope, principalID, executionID, version, deadline, credential, now)
}

// renew atomically replaces the committed credential through the token manager
// and delivers the replacement. The manager caps the lifetime by the action
// deadline and the maximum TTL. A failed delivery keeps the committed token
// for a later retry instead of minting again.
func (loop *renewalLoop) renew(
	ctx context.Context,
	scope tenancy.Scope,
	principalID id.PrincipalID,
	executionID id.ExecutionID,
	version uint64,
	deadline time.Time,
	credential token.Credential,
	now time.Time,
) {
	record, err := loop.renewalReceipt(scope, principalID, executionID, version, credential, now)
	if err != nil {
		loop.logFault(executionID, err)
		return
	}
	result, err := loop.manager.Rotate(ctx, token.IssueRequest{
		Scope:           scope,
		PrincipalID:     principalID,
		ExecutionID:     executionID,
		TTL:             token.DefaultBounds.MaxTTL,
		Deadline:        deadline,
		ExpectedVersion: version,
		Receipt:         record,
	})
	if err != nil {
		if result.Delivery != nil {
			// The commit succeeded and only the post-commit delivery failed.
			// Retry this exact token; never re-mint.
			loop.rememberPending(ctx, scope, principalID, executionID, *result.Delivery)
		}
		loop.logFault(executionID, err)
		return
	}
	// A replayed commit minted nothing and delivered nothing. A successful
	// rotate already delivered the replacement through the same seam, so a
	// successful return needs no further work.
}

// rememberPending records a committed-but-undelivered renewal token. It reads
// the committed nonce so a later pass can prove the token still names the
// current generation before retrying delivery. An unreadable fact fails closed:
// the token is not retried rather than delivered after the credential changed.
func (loop *renewalLoop) rememberPending(
	ctx context.Context,
	scope tenancy.Scope,
	principalID id.PrincipalID,
	executionID id.ExecutionID,
	delivery token.Delivery,
) {
	fact, err := loop.credentials.ReadCredential(ctx, token.CredentialRequest{
		Scope:       scope,
		PrincipalID: principalID,
		ExecutionID: executionID,
	})
	if err != nil {
		loop.logFault(executionID, err)
		return
	}
	if !fact.Enabled || !fact.Credential.Present() {
		return
	}
	loop.pending[executionID] = pendingRenewal{
		scope:       scope,
		principalID: principalID,
		executionID: executionID,
		nonce:       append([]byte(nil), fact.Credential.Nonce...),
		delivery:    delivery,
	}
}

// retryPending delivers each committed-but-undelivered renewal token whose
// committed generation is still current. One execution's failure never blocks
// another, and the loop never mints here.
func (loop *renewalLoop) retryPending(ctx context.Context, now time.Time) {
	for executionID, pending := range loop.pending {
		if err := ctx.Err(); err != nil {
			return
		}
		fact, err := loop.credentials.ReadCredential(ctx, token.CredentialRequest{
			Scope:       pending.scope,
			PrincipalID: pending.principalID,
			ExecutionID: pending.executionID,
		})
		if err != nil {
			loop.logFault(executionID, err)
			continue
		}
		if !fact.Enabled || !fact.Credential.Present() ||
			fact.Credential.State(now) != token.StateCurrent ||
			!bytes.Equal(fact.Credential.Nonce, pending.nonce) {
			// The execution stopped, the credential was revoked or replaced, or
			// the token expired: never deliver a credential that cannot
			// authenticate.
			delete(loop.pending, executionID)
			continue
		}
		if err := loop.deliverer.Deliver(ctx, pending.delivery); err != nil {
			if ctx.Err() != nil {
				return
			}
			loop.logFault(executionID, err)
			continue
		}
		delete(loop.pending, executionID)
	}
}

// actionDeadline reads the persisted deadline of the execution's current
// running action. The deadline is an upper bound on every renewal lifetime.
func (loop *renewalLoop) actionDeadline(ctx context.Context, executionID id.ExecutionID) (time.Time, bool, error) {
	page, err := loop.controller.Scan(ctx, store.ControllerScan{
		Table: "actions",
		Equals: map[string]any{
			"execution_id": string(executionID),
			"state":        "running",
		},
		OrderBy: []store.Order{{Column: "index"}},
		Limit:   1})
	if err != nil {
		return time.Time{}, false, err
	}
	if len(page.Rows) == 0 {
		return time.Time{}, false, nil
	}
	deadline, err := rowTime(page.Rows[0], "deadline")
	if err != nil {
		return time.Time{}, false, err
	}
	if deadline.IsZero() {
		return time.Time{}, false, nil
	}
	return deadline, true, nil
}

// renewalReceipt builds the durable, secret-free receipt of one renewal commit.
// The caller key names the committed generation being replaced, not just its
// expiry: the nonce changes on every committed pair, so a repeated rotate of the
// same generation replays instead of minting again, while any later generation
// (including one that inherited a deadline-capped expiry from an older
// composition) gets its own receipt. An expiry-only key could be masked by the
// receipt of a pointless D-to-D rotation.
func (loop *renewalLoop) renewalReceipt(
	scope tenancy.Scope,
	principalID id.PrincipalID,
	executionID id.ExecutionID,
	version uint64,
	credential token.Credential,
	now time.Time,
) (receipt.Record, error) {
	generation := strconv.FormatInt(credential.Expiry.UnixNano(), 10)
	fingerprint, err := receipt.Fingerprint(receipt.Content{
		MediaType: "application/octet-stream",
		Bytes:     []byte(string(executionID) + "\x00" + generation + "\x00" + string(credential.Nonce)),
	})
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "renewal receipt content is invalid", err)
	}
	receiptID, err := loop.ids.NewReceiptID()
	if err != nil {
		return receipt.Record{}, err
	}
	result, err := json.Marshal(token.IssueResult{State: token.StateCurrent, Version: version + 1})
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "renewal receipt result could not be encoded", err)
	}
	return receipt.Record{
		ID: receiptID,
		Key: receipt.Key{
			TenantID:    scope.TenantID,
			ProjectID:   scope.ProjectID,
			PrincipalID: principalID,
			Operation:   renewalCredentialOperation,
			CallerKey:   "renewal:" + string(executionID) + ":" + generation + ":" + fingerprint,
		},
		ContentHash: fingerprint,
		Result:      result,
		ExecutionID: executionID,
		At:          now,
	}, nil
}

// logFault logs one stable, secret-free renewal failure. Only the fault
// class is rendered; the adapter error text and credential material never are.
func (loop *renewalLoop) logFault(executionID id.ExecutionID, err error) {
	if loop.logger == nil {
		return
	}
	if executionID == "" {
		loop.logger.Warn("credential renewal scan failed", "detail", loopFailureDetail(err))
		return
	}
	loop.logger.Warn("credential renewal failed",
		"execution", string(executionID),
		"detail", loopFailureDetail(err))
}

// renewalExecutionFact reads the owning scope, principal, row version and
// committed credential of one scanned execution row.
func renewalExecutionFact(row store.Row) (tenancy.Scope, id.PrincipalID, uint64, token.Credential, error) {
	tenantID, err := rowText(row, "tenant_id")
	if err != nil {
		return tenancy.Scope{}, "", 0, token.Credential{}, err
	}
	projectID, err := rowText(row, "project_id")
	if err != nil {
		return tenancy.Scope{}, "", 0, token.Credential{}, err
	}
	principalID, err := rowText(row, "principal_id")
	if err != nil {
		return tenancy.Scope{}, "", 0, token.Credential{}, err
	}
	version, err := rowUint64(row, "version")
	if err != nil {
		return tenancy.Scope{}, "", 0, token.Credential{}, err
	}
	nonce, err := rowBytes(row, "token_nonce")
	if err != nil {
		return tenancy.Scope{}, "", 0, token.Credential{}, err
	}
	expiry, err := rowTime(row, "token_expires_at")
	if err != nil {
		return tenancy.Scope{}, "", 0, token.Credential{}, err
	}
	scope := tenancy.Scope{TenantID: id.TenantID(tenantID), ProjectID: id.ProjectID(projectID)}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, "", 0, token.Credential{}, fault.Wrap(fault.DependencyUnavailable, "renewal execution scope is invalid", err)
	}
	return scope, id.PrincipalID(principalID), version, token.Credential{Nonce: nonce, Expiry: expiry}, nil
}

// rowBytes reads one bytea column. A null column is an absent value, not an
// error.
func rowBytes(row store.Row, column string) ([]byte, error) {
	value, ok := row.Values[column]
	if !ok || value == nil {
		return nil, nil
	}
	raw, ok := value.([]byte)
	if !ok {
		return nil, fault.New(fault.DependencyUnavailable, "renewal row byte column has an unexpected type")
	}
	return append([]byte(nil), raw...), nil
}

// rowTime reads one timestamp column. A null column is the zero time.
func rowTime(row store.Row, column string) (time.Time, error) {
	value, ok := row.Values[column]
	if !ok || value == nil {
		return time.Time{}, nil
	}
	moment, ok := value.(time.Time)
	if !ok {
		return time.Time{}, fault.New(fault.DependencyUnavailable, "renewal row time column has an unexpected type")
	}
	return moment, nil
}

// rowUint64 reads one optimistic-version column across the integer shapes an
// adapter may materialize.
func rowUint64(row store.Row, column string) (uint64, error) {
	switch value := row.Values[column].(type) {
	case uint64:
		return value, nil
	case uint32:
		return uint64(value), nil
	case int64:
		if value < 0 {
			return 0, fault.New(fault.DependencyUnavailable, "renewal row version is negative")
		}
		return uint64(value), nil
	case int:
		if value < 0 {
			return 0, fault.New(fault.DependencyUnavailable, "renewal row version is negative")
		}
		return uint64(value), nil
	default:
		return 0, fault.New(fault.DependencyUnavailable, "renewal row version has an unexpected type")
	}
}
