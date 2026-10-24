// Package exceptions owns inventory-exception review and the rules for
// resolving one. The ERP is authoritative for balances: a local exception is
// resolved only after the ERP acknowledges the reviewed adjustment, and a
// pending or unknown operation is never reported as resolved.
package exceptions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"example.invalid/inventory/internal/tenancy"
)

// State is the exception lifecycle: open -> pending -> resolved | failed | unknown,
// and unknown -> resolved | failed after reconciliation.
type State string

// States.
const (
	StateOpen     State = "open"
	StatePending  State = "pending"
	StateResolved State = "resolved"
	StateFailed   State = "failed"
	StateUnknown  State = "unknown"
)

// Valid reports whether s is defined.
func (s State) Valid() bool {
	switch s {
	case StateOpen, StatePending, StateResolved, StateFailed, StateUnknown:
		return true
	}
	return false
}

// Terminal reports whether no further transition exists.
func (s State) Terminal() bool { return s == StateResolved || s == StateFailed }

// Errors, mapped to machine-readable codes by the transport.
var (
	ErrInvalid             = errors.New("exceptions: invalid value")
	ErrNotFound            = errors.New("exceptions: not found")
	ErrVersionConflict     = errors.New("exceptions: version conflict")
	ErrInvalidState        = errors.New("exceptions: transition not allowed in this state")
	ErrIdempotencyConflict = errors.New("exceptions: idempotency key reused with a different request")
	ErrOperationMismatch   = errors.New("exceptions: observation names a different operation")
	ErrLeaseLost           = errors.New("exceptions: the claim is no longer held")
	ErrUnavailable         = errors.New("exceptions: dependency unavailable")
)

// Limits.
const (
	MaxReasonBytes = 500
	MaxQuantity    = 1_000_000_000
)

var (
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	keyRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	refRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,127}$`)
)

// ID identifies an exception within its tenant.
type ID string

// ParseID validates an exception identifier.
func ParseID(s string) (ID, error) {
	if !uuidRE.MatchString(s) {
		return "", fmt.Errorf("%w: exception id must be a lowercase UUID", ErrInvalid)
	}
	return ID(s), nil
}

// Resolution is the reviewed adjustment and what is known about its ERP outcome.
type Resolution struct {
	// OperationKey is the stable ERP idempotency/reconciliation key, derived
	// from the exception identity so every retry names the same remote effect.
	OperationKey     string
	AdjustedQuantity int64
	Reason           string
	RequestedBy      string
	PayloadHash      string
	// ERPReference is the ERP's acknowledgment reference once known.
	ERPReference string
	// Detail explains a rejection or an uncertain outcome.
	Detail string
}

// Exception is an inventory gap between the ERP balance and a count.
type Exception struct {
	ID              ID
	Tenant          tenancy.TenantID
	ERPRef          string
	Description     string
	ERPQuantity     int64
	CountedQuantity int64
	State           State
	Version         int64
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Resolution      *Resolution
}

// NewException validates and opens an exception at version 1.
func NewException(tenant tenancy.TenantID, id ID, erpRef, description string, erpQty, countedQty int64, createdBy string, now time.Time) (Exception, error) {
	if _, err := tenancy.ParseTenantID(string(tenant)); err != nil {
		return Exception{}, fmt.Errorf("%w: tenant", ErrInvalid)
	}
	if _, err := ParseID(string(id)); err != nil {
		return Exception{}, err
	}
	if !refRE.MatchString(erpRef) {
		return Exception{}, fmt.Errorf("%w: erpRef must be 1..128 characters of letters, digits and ._/:-", ErrInvalid)
	}
	if len(description) > 1000 {
		return Exception{}, fmt.Errorf("%w: description is at most 1000 bytes", ErrInvalid)
	}
	for _, q := range []int64{erpQty, countedQty} {
		if q < 0 || q > MaxQuantity {
			return Exception{}, fmt.Errorf("%w: quantities are integers within 0..%d", ErrInvalid, MaxQuantity)
		}
	}
	if erpQty == countedQty {
		return Exception{}, fmt.Errorf("%w: there is no gap between the ERP and counted quantities", ErrInvalid)
	}
	if createdBy == "" || len(createdBy) > 256 {
		return Exception{}, fmt.Errorf("%w: an attributable actor is required", ErrInvalid)
	}
	return Exception{ID: id, Tenant: tenant, ERPRef: erpRef, Description: description, ERPQuantity: erpQty, CountedQuantity: countedQty,
		State: StateOpen, Version: 1, CreatedBy: createdBy, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

// ResolveRequest is a reviewer's command to resolve one exception.
type ResolveRequest struct {
	Actor            string
	AdjustedQuantity int64
	Reason           string
	IdempotencyKey   string
	ExpectedVersion  int64
}

// Validate checks the request independently of any exception.
func (r ResolveRequest) Validate() error {
	switch {
	case r.Actor == "" || len(r.Actor) > 256:
		return fmt.Errorf("%w: an attributable actor is required", ErrInvalid)
	case r.AdjustedQuantity < 0 || r.AdjustedQuantity > MaxQuantity:
		return fmt.Errorf("%w: adjustedQuantity must be within 0..%d", ErrInvalid, MaxQuantity)
	case r.Reason == "" || len(r.Reason) > MaxReasonBytes:
		return fmt.Errorf("%w: reason is required and at most %d bytes", ErrInvalid, MaxReasonBytes)
	case !keyRE.MatchString(r.IdempotencyKey):
		return fmt.Errorf("%w: Idempotency-Key must be 8..128 characters of letters, digits and ._:-", ErrInvalid)
	case r.ExpectedVersion < 1:
		return fmt.Errorf("%w: the expected version is required", ErrInvalid)
	}
	return nil
}

// PayloadHash identifies the meaning of the request so a reused idempotency key
// with different content is detectable. The actor and key are not part of it.
func (r ResolveRequest) PayloadHash(id ID) string {
	raw, _ := json.Marshal(struct {
		Exception string `json:"exception"`
		Quantity  int64  `json:"adjustedQuantity"`
		Reason    string `json:"reason"`
		Version   int64  `json:"expectedVersion"`
	}{string(id), r.AdjustedQuantity, r.Reason, r.ExpectedVersion})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Intent is the durable instruction to apply an adjustment in the ERP.
type Intent struct {
	ID               string
	Tenant           tenancy.TenantID
	ExceptionID      ID
	OperationKey     string
	ERPRef           string
	AdjustedQuantity int64
	Reason           string
	PayloadHash      string
	Attempts         int
}

// OperationKeyFor derives the stable ERP operation key of an exception.
func OperationKeyFor(id ID) string { return "inv-adj-" + string(id) }

// RequestResolution moves open -> pending and returns the durable intent. The
// caller persists both atomically. A changed version is a conflict; a request
// in any state but open cannot be accepted.
func (e *Exception) RequestResolution(req ResolveRequest, now time.Time) (Intent, error) {
	if err := req.Validate(); err != nil {
		return Intent{}, err
	}
	if req.ExpectedVersion != e.Version {
		return Intent{}, ErrVersionConflict
	}
	if e.State != StateOpen {
		return Intent{}, fmt.Errorf("%w: exception is %s", ErrInvalidState, e.State)
	}
	hash := req.PayloadHash(e.ID)
	e.Resolution = &Resolution{OperationKey: OperationKeyFor(e.ID), AdjustedQuantity: req.AdjustedQuantity, Reason: req.Reason, RequestedBy: req.Actor, PayloadHash: hash}
	e.touch(StatePending, now)
	return Intent{Tenant: e.Tenant, ExceptionID: e.ID, OperationKey: e.Resolution.OperationKey, ERPRef: e.ERPRef,
		AdjustedQuantity: req.AdjustedQuantity, Reason: req.Reason, PayloadHash: hash}, nil
}

func (e *Exception) touch(s State, now time.Time) {
	e.State = s
	e.Version++
	e.UpdatedAt = now.UTC()
}

func (e *Exception) checkOperation(key string) error {
	if e.Resolution == nil || e.Resolution.OperationKey != key {
		return ErrOperationMismatch
	}
	return nil
}

// Acknowledge records the ERP's acknowledgment: pending|unknown -> resolved.
// Replaying the same acknowledgment on a resolved exception changes nothing.
func (e *Exception) Acknowledge(key, erpReference string, now time.Time) error {
	if err := e.checkOperation(key); err != nil {
		return err
	}
	if erpReference == "" || len(erpReference) > 256 {
		return fmt.Errorf("%w: an acknowledgment carries the ERP reference", ErrInvalid)
	}
	if e.State == StateResolved && e.Resolution.ERPReference == erpReference {
		return nil
	}
	if e.State != StatePending && e.State != StateUnknown {
		return fmt.Errorf("%w: cannot acknowledge a %s exception", ErrInvalidState, e.State)
	}
	e.Resolution.ERPReference = erpReference
	e.Resolution.Detail = ""
	e.touch(StateResolved, now)
	return nil
}

// Reject records a definite ERP refusal: pending|unknown -> failed.
func (e *Exception) Reject(key, reason string, now time.Time) error {
	if err := e.checkOperation(key); err != nil {
		return err
	}
	if e.State == StateFailed && e.Resolution.Detail == reason {
		return nil
	}
	if e.State != StatePending && e.State != StateUnknown {
		return fmt.Errorf("%w: cannot reject a %s exception", ErrInvalidState, e.State)
	}
	e.Resolution.Detail = reason
	e.touch(StateFailed, now)
	return nil
}

// MarkUnknown records that the ERP outcome cannot be established yet:
// pending -> unknown. A repeated report keeps the state and refreshes the detail.
func (e *Exception) MarkUnknown(key, detail string, now time.Time) error {
	if err := e.checkOperation(key); err != nil {
		return err
	}
	switch e.State {
	case StateUnknown:
		e.Resolution.Detail = detail
		return nil
	case StatePending:
		e.Resolution.Detail = detail
		e.touch(StateUnknown, now)
		return nil
	}
	return fmt.Errorf("%w: cannot mark a %s exception unknown", ErrInvalidState, e.State)
}
