// Package service declares service-account values, credential metadata and the
// complete service-administration commit units.
//
// A service account is a scoped, enabled principal whose roles never include
// Admin. Its derived credential is one current nullable nonce/expiry pair; the
// Shell stores neither token plaintext nor a token hash, so this value carries
// the credential fields rather than any bearer string.
//
// Service creation, role update and enablement/disablement are three complete
// atomic commit units applied by AdminPort. A disablement carries the mandatory
// RevokeAll obligation to clear the service's one current nonce/expiry pair in
// the same transaction as the disabled state, guarded by the expected service
// version on that row, so it cannot leave a live credential behind; an
// enablement never carries a credential and therefore cannot resurrect a
// revoked one. Credential issue/rotate/revoke stay on token.MutationUnit and
// are deliberately absent here.
package service

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// MinTokenNonceBytes is the minimum credential nonce length. Each tenant key
// and principal nonce has at least 256 random bits (S05).
const MinTokenNonceBytes = 32

// ServiceID identifies one service account.
type ServiceID string

// ParseServiceID validates and converts a service-account identity.
func ParseServiceID(value string) (ServiceID, error) {
	if !validServiceID(value) {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("invalid service id %q", value))
	}
	return ServiceID(value), nil
}

func validServiceID(value string) bool {
	if value == "" || len(value) > id.MaxIDLength {
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

// State is the enablement state of a service account.
type State string

const (
	// StateEnabled means the service may authenticate and act.
	StateEnabled State = "enabled"
	// StateDisabled means the service may not authenticate and its credential
	// fields are cleared.
	StateDisabled State = "disabled"
)

var states = [...]State{StateEnabled, StateDisabled}

// States returns every service state in declaration order.
func States() []State {
	return append([]State(nil), states[:]...)
}

// Valid reports whether the state is one of the declared states.
func (state State) Valid() bool {
	for _, candidate := range states {
		if state == candidate {
			return true
		}
	}
	return false
}

// Service is one service account and its current credential metadata.
//
// TokenNonce and TokenExpiry are both present or both absent. A present nonce
// is at least MinTokenNonceBytes long. The token plaintext is never a field
// here: it exists only for authorized delivery.
type Service struct {
	ID          ServiceID      `json:"id"`
	Scope       tenancy.Scope  `json:"scope"`
	State       State          `json:"state"`
	Roles       []tenancy.Role `json:"roles"`
	Version     uint64         `json:"version"`
	TokenNonce  []byte         `json:"-"`
	TokenExpiry *time.Time     `json:"-"`
}

// Validate checks the service identity, scope, state, roles and credential
// pair. Every failure uses the declared invalid_input class.
func (service Service) Validate() error {
	if _, err := ParseServiceID(string(service.ID)); err != nil {
		return err
	}
	if err := service.Scope.Validate(); err != nil {
		return err
	}
	if !service.State.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown service state %q", service.State))
	}
	for _, role := range service.Roles {
		if !role.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown service role %q", role))
		}
		if !role.ServiceAssignable() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("service role %q is not service-assignable", role))
		}
	}
	noncePresent := len(service.TokenNonce) > 0
	expiryPresent := service.TokenExpiry != nil
	if noncePresent != expiryPresent {
		return fault.New(fault.InvalidInput, "service credential nonce and expiry must both be present or both be absent")
	}
	if noncePresent && len(service.TokenNonce) < MinTokenNonceBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("service credential nonce is %d bytes; at least %d are required", len(service.TokenNonce), MinTokenNonceBytes))
	}
	if expiryPresent && service.TokenExpiry.IsZero() {
		return fault.New(fault.InvalidInput, "service credential expiry is zero")
	}
	return nil
}

// Active reports whether the service is enabled. Credential presence is a
// separate fact; authorization rechecks both at the commit boundary.
func (service Service) Active() bool {
	return service.State == StateEnabled
}

// CreateUnit is the complete atomic service-creation commit unit. The created
// service starts at version 1 with no credential pair; credential issuance is a
// separate token.MutationUnit. The adapter re-reads the supplied current
// authority inside the same transaction as the service row, receipt, audit and
// event publication intent.
type CreateUnit struct {
	Owner      tenancy.Scope             `json:"owner"`
	Service    Service                   `json:"service"`
	Actor      id.PrincipalID            `json:"actor"`
	At         time.Time                 `json:"at"`
	Receipt    receipt.Record            `json:"receipt"`
	Audit      []event.Audit             `json:"audit"`
	Events     []event.Envelope          `json:"events"`
	Authority  []authorize.AuthorityRead `json:"authority"`
	PostCommit []event.PostCommitEffect  `json:"post_commit"`
}

// Validate checks the owner scope, the version-1 service, the absent
// credential pair, the acting principal and the commit time. An incomplete
// create unit is refused instead of creating a partial service row.
func (unit CreateUnit) Validate() error {
	if err := unit.Owner.Validate(); err != nil {
		return err
	}
	if err := unit.Service.Validate(); err != nil {
		return err
	}
	if unit.Service.Scope != unit.Owner {
		return fault.New(fault.InvalidInput, "service create scope does not match the service scope")
	}
	if unit.Service.Version != 1 {
		return fault.New(fault.InvalidInput, "service creation must start at version 1")
	}
	if serviceHasCredential(unit.Service) {
		return fault.New(fault.InvalidInput, "service creation must not carry a credential pair")
	}
	if err := validateAdminActor(unit.Actor); err != nil {
		return err
	}
	if unit.At.IsZero() {
		return fault.New(fault.InvalidInput, "service creation time is zero")
	}
	return nil
}

// CreateResult is the committed service-creation result.
type CreateResult struct {
	ServiceID ServiceID `json:"service_id"`
	Version   uint64    `json:"version"`
	Replayed  bool      `json:"replayed"`
}

// UpdateUnit is the complete atomic service role-update commit unit.
//
// ExpectedState guards enablement: the adapter compares it to the current row
// and refuses a state change, so enable/disable must use StatusUnit. Service is
// the complete next value and must advance ExpectedVersion by exactly one. It
// never carries a credential pair.
type UpdateUnit struct {
	Owner           tenancy.Scope             `json:"owner"`
	ServiceID       ServiceID                 `json:"service_id"`
	ExpectedVersion uint64                    `json:"expected_version"`
	ExpectedState   State                     `json:"expected_state"`
	Service         Service                   `json:"service"`
	Actor           id.PrincipalID            `json:"actor"`
	At              time.Time                 `json:"at"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// Validate checks the owner scope, service identity, expected version and
// state, the next complete service value, the absent credential pair, the
// acting principal and the commit time.
func (unit UpdateUnit) Validate() error {
	if err := unit.Owner.Validate(); err != nil {
		return err
	}
	if _, err := ParseServiceID(string(unit.ServiceID)); err != nil {
		return err
	}
	if unit.ExpectedVersion == 0 {
		return fault.New(fault.InvalidInput, "service update expected version is zero")
	}
	if !unit.ExpectedState.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown expected service state %q", unit.ExpectedState))
	}
	if err := unit.Service.Validate(); err != nil {
		return err
	}
	if unit.Service.ID != unit.ServiceID {
		return fault.New(fault.InvalidInput, "service update id does not match the service value")
	}
	if unit.Service.Scope != unit.Owner {
		return fault.New(fault.InvalidInput, "service update scope does not match the service scope")
	}
	if unit.Service.Version != unit.ExpectedVersion+1 {
		return fault.New(fault.InvalidInput, "service update does not advance the expected version")
	}
	if unit.Service.State != unit.ExpectedState {
		return fault.New(fault.InvalidInput, "service update must not change enablement; use StatusUnit")
	}
	if serviceHasCredential(unit.Service) {
		return fault.New(fault.InvalidInput, "service update must not carry a credential pair")
	}
	if err := validateAdminActor(unit.Actor); err != nil {
		return err
	}
	if unit.At.IsZero() {
		return fault.New(fault.InvalidInput, "service update time is zero")
	}
	return nil
}

// UpdateResult is the committed service role-update result.
type UpdateResult struct {
	Version  uint64 `json:"version"`
	Replayed bool   `json:"replayed"`
}

// RevokeAll is the mandatory credential-clearing obligation of a service
// disablement. A disablement clears the service's one current nonce/expiry pair
// in the same transaction as the disabled state; StatusUnit.ExpectedVersion on
// that same service row is the stale-read guard. The obligation is
// unconditional and therefore carries no observed value: a disablement is valid
// whether or not the service currently has a credential. An enablement carries
// no RevokeAll and cannot resurrect a revoked credential.
type RevokeAll struct{}

// StatusUnit is the complete atomic service enablement/disablement commit unit.
// Disabling carries the mandatory RevokeAll obligation; enabling carries none
// and never invents a credential pair.
type StatusUnit struct {
	Owner           tenancy.Scope             `json:"owner"`
	ServiceID       ServiceID                 `json:"service_id"`
	ExpectedVersion uint64                    `json:"expected_version"`
	State           State                     `json:"state"`
	RevokeAll       *RevokeAll                `json:"revoke_all,omitempty"`
	Actor           id.PrincipalID            `json:"actor"`
	At              time.Time                 `json:"at"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// Validate checks the owner scope, service identity, expected version, next
// state, revocation obligation, acting principal and commit time. The
// obligation follows the next state: a disablement requires RevokeAll and an
// enablement must not carry one. No observed credential value is required, so a
// disablement is valid for a service that has never held a credential.
func (unit StatusUnit) Validate() error {
	if err := unit.Owner.Validate(); err != nil {
		return err
	}
	if _, err := ParseServiceID(string(unit.ServiceID)); err != nil {
		return err
	}
	if unit.ExpectedVersion == 0 {
		return fault.New(fault.InvalidInput, "service status expected version is zero")
	}
	if !unit.State.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown service state %q", unit.State))
	}
	if unit.State == StateDisabled {
		if unit.RevokeAll == nil {
			return fault.New(fault.InvalidInput, "service disablement requires the RevokeAll credential-clearing obligation")
		}
	} else if unit.RevokeAll != nil {
		return fault.New(fault.InvalidInput, "service enablement must not carry a credential revocation")
	}
	if err := validateAdminActor(unit.Actor); err != nil {
		return err
	}
	if unit.At.IsZero() {
		return fault.New(fault.InvalidInput, "service status time is zero")
	}
	return nil
}

// StatusResult is the committed service enablement/disablement result.
// RevokedCredentials reports whether the disablement cleared the service's one
// stored nonce/expiry pair: 1 when a pair was present and 0 otherwise. An
// enablement reports zero.
type StatusResult struct {
	State              State  `json:"state"`
	Version            uint64 `json:"version"`
	RevokedCredentials int    `json:"revoked_credentials"`
	Replayed           bool   `json:"replayed"`
}

func serviceHasCredential(value Service) bool {
	return len(value.TokenNonce) > 0 || value.TokenExpiry != nil
}

func validateAdminActor(actor id.PrincipalID) error {
	if _, err := id.ParsePrincipalID(string(actor)); err != nil {
		return fault.Wrap(fault.InvalidInput, "service admin actor principal id is invalid", err)
	}
	return nil
}
