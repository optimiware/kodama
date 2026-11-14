// Package token declares derived credential values, the canonical binary MAC
// input and the atomic credential-mutation commit unit.
//
// A tenant-key HMAC-SHA256 over an unambiguous canonical encoding yields either
//
//	kodex.<execution-id>.<base64url-no-padding(mac)>
//	kodsvc.<service-id>.<base64url-no-padding(mac)>
//
// The encoding length-prefixes the domain, tenant, principal and nonce and
// appends the big-endian uint64 Unix-seconds expiry. IDs in the presented token
// are lookup hints only until the constant-time MAC check succeeds. The Shell
// stores neither token plaintext nor a token hash; a principal has one current
// nullable nonce/expiry pair, so revocation is a committed mutation, never a
// cached allow.
package token

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Domain separators of the two derived token kinds. The encoded domain string
// is the first length-prefixed field of the MAC input.
const (
	// ExecutionDomain is the execution credential domain.
	ExecutionDomain = "kodama-execution-v1"
	// ServiceDomain is the service credential domain.
	ServiceDomain = "kodama-service-v1"
)

// Presented token prefixes. A token of the wrong prefix/kind is refused.
const (
	// ExecutionPrefix precedes an execution credential.
	ExecutionPrefix = "kodex."
	// ServicePrefix precedes a service credential.
	ServicePrefix = "kodsvc."
)

// MinNonceBytes is the minimum nonce length: at least 256 random bits.
const MinNonceBytes = 32

// State is the derived state of one stored credential pair.
type State string

const (
	// StateAbsent means no nonce/expiry pair is stored.
	StateAbsent State = "absent"
	// StateCurrent means the stored nonce is present and not yet expired.
	StateCurrent State = "current"
	// StateExpired means the stored expiry has been reached.
	StateExpired State = "expired"
)

var states = [...]State{StateAbsent, StateCurrent, StateExpired}

// Valid reports whether the state is one of the declared states.
func (state State) Valid() bool {
	for _, candidate := range states {
		if state == candidate {
			return true
		}
	}
	return false
}

// Credential is one current nonce/expiry pair. It never carries token
// plaintext or a token hash.
type Credential struct {
	Nonce  []byte    `json:"-"`
	Expiry time.Time `json:"expiry"`
}

// Present reports whether a complete nonce/expiry pair is stored.
func (credential Credential) Present() bool {
	return len(credential.Nonce) > 0 && !credential.Expiry.IsZero()
}

// State derives absent/current/expired at the supplied authoritative time. The
// boundary is inclusive: an expiry equal to now is expired.
func (credential Credential) State(now time.Time) State {
	if !credential.Present() {
		return StateAbsent
	}
	if !now.Before(credential.Expiry) {
		return StateExpired
	}
	return StateCurrent
}

// ExecutionClaims is the canonical input of an execution credential.
type ExecutionClaims struct {
	TenantID    id.TenantID
	ExecutionID id.ExecutionID
	Nonce       []byte
	Expiry      time.Time
}

// ServiceClaims is the canonical input of a service credential.
type ServiceClaims struct {
	TenantID  id.TenantID
	ServiceID service.ServiceID
	Nonce     []byte
	Expiry    time.Time
}

// ExecutionMACInput returns the canonical binary MAC input for an execution
// credential. Validation refuses missing fields, short nonces and a non-positive
// expiry rather than encoding them ambiguously.
func ExecutionMACInput(claims ExecutionClaims) ([]byte, error) {
	if _, err := id.ParseTenantID(string(claims.TenantID)); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "execution credential tenant id is invalid", err)
	}
	if _, err := id.ParseExecutionID(string(claims.ExecutionID)); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "execution credential execution id is invalid", err)
	}
	return encodeMACInput(ExecutionDomain, string(claims.TenantID), string(claims.ExecutionID), claims.Nonce, claims.Expiry)
}

// ServiceMACInput returns the canonical binary MAC input for a service
// credential.
func ServiceMACInput(claims ServiceClaims) ([]byte, error) {
	if _, err := id.ParseTenantID(string(claims.TenantID)); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "service credential tenant id is invalid", err)
	}
	if _, err := service.ParseServiceID(string(claims.ServiceID)); err != nil {
		return nil, err
	}
	return encodeMACInput(ServiceDomain, string(claims.TenantID), string(claims.ServiceID), claims.Nonce, claims.Expiry)
}

func encodeMACInput(domain, tenant, principal string, nonce []byte, expiry time.Time) ([]byte, error) {
	if len(nonce) < MinNonceBytes {
		return nil, fault.New(fault.InvalidInput, "credential nonce is shorter than the minimum")
	}
	if expiry.Unix() <= 0 {
		return nil, fault.New(fault.InvalidInput, "credential expiry is required and must be after the Unix epoch")
	}
	fields := [][]byte{[]byte(domain), []byte(tenant), []byte(principal), nonce}
	var buffer bytes.Buffer
	buffer.Grow(4*len(fields) + len(nonce) + 8)
	for _, field := range fields {
		if uint64(len(field)) > math.MaxUint32 {
			return nil, fault.New(fault.InvalidInput, "credential field is too long to encode")
		}
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		buffer.Write(length[:])
		buffer.Write(field)
	}
	var encodedExpiry [8]byte
	binary.BigEndian.PutUint64(encodedExpiry[:], uint64(expiry.Unix()))
	buffer.Write(encodedExpiry[:])
	return buffer.Bytes(), nil
}

// Bounds are the installation credential lifetime bounds. Both ends are
// inclusive, so a TTL exactly at MaxTTL is permitted; S05 caps execution
// credentials at eight hours, which is the declared maximum.
type Bounds struct {
	MinNonceBytes int           `json:"min_nonce_bytes"`
	MinTTL        time.Duration `json:"min_ttl"`
	MaxTTL        time.Duration `json:"max_ttl"`
}

// DefaultBounds is the declared credential bound set. MinTTL stays below the
// one-minute minimum action timeout so an action with `timeout: 1m` retains
// headroom to mint its execution credential after activation commits its
// deadline.
var DefaultBounds = Bounds{
	MinNonceBytes: MinNonceBytes,
	MinTTL:        15 * time.Second,
	MaxTTL:        8 * time.Hour,
}

// ValidTTL reports whether ttl is within [MinTTL, MaxTTL].
func (bounds Bounds) ValidTTL(ttl time.Duration) bool {
	return ttl >= bounds.MinTTL && ttl <= bounds.MaxTTL
}

// MutationKind names one credential lifecycle mutation.
type MutationKind string

const (
	// MutationIssue creates a first credential pair.
	MutationIssue MutationKind = "issue"
	// MutationRotate atomically replaces the current pair.
	MutationRotate MutationKind = "rotate"
	// MutationRevoke clears the current pair.
	MutationRevoke MutationKind = "revoke"
)

var mutationKinds = [...]MutationKind{MutationIssue, MutationRotate, MutationRevoke}

// Valid reports whether the kind is one of the declared kinds.
func (kind MutationKind) Valid() bool {
	for _, candidate := range mutationKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// MutationUnit is the complete atomic per-principal credential-mutation commit
// unit.
//
// The adapter checks the expected principal version and supplied current
// authority inside the same transaction as the nonce/expiry replacement or
// clearing, receipt, audit and publication intent. Delivery of a new token is a
// post-commit effect; no plaintext is part of this value.
//
// Service is an optional companion: a service mutation carries the next
// enablement state and/or role set so service administration commits with the
// credential pair instead of a second store call. It never carries plaintext
// token material. A tenant derivation-key rotation is not a per-principal
// credential mutation and is committed by the dedicated
// tenancyadmin.TenantKeyRotationUnit, so this unit has no tenant-key field and
// cannot ambiguously mix the two authority targets.
type MutationUnit struct {
	Owner           tenancy.Scope             `json:"owner"`
	PrincipalID     id.PrincipalID            `json:"principal_id"`
	ExecutionID     id.ExecutionID            `json:"execution_id,omitempty"`
	ServiceID       service.ServiceID         `json:"service_id,omitempty"`
	Kind            MutationKind              `json:"kind"`
	ExpectedVersion uint64                    `json:"expected_version"`
	Credential      Credential                `json:"credential"`
	Service         *ServiceChange            `json:"service,omitempty"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// ServiceChange is the optional service-account administration committed with
// a credential mutation. A nil State leaves enablement unchanged; a nil Roles
// slice leaves the current roles unchanged. A non-nil empty Roles slice clears
// every role.
type ServiceChange struct {
	State *service.State `json:"state,omitempty"`
	Roles []tenancy.Role `json:"roles,omitempty"`
}

// Validate checks the optional state and every supplied role. Services never
// hold Admin.
func (change ServiceChange) Validate() error {
	if change.State != nil && !change.State.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown service state %q", *change.State))
	}
	for _, role := range change.Roles {
		if !role.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown service role %q", role))
		}
		if !role.ServiceAssignable() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("service role %q is not service-assignable", role))
		}
	}
	return nil
}

// CredentialRequest asks for the current credential pair of exactly one
// execution or service principal in one scope. Naming both targets, or
// neither, is refused.
type CredentialRequest struct {
	Scope       tenancy.Scope     `json:"scope"`
	PrincipalID id.PrincipalID    `json:"principal_id"`
	ExecutionID id.ExecutionID    `json:"execution_id,omitempty"`
	ServiceID   service.ServiceID `json:"service_id,omitempty"`
}

// Validate checks the scope, principal and exactly one target.
func (request CredentialRequest) Validate() error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParsePrincipalID(string(request.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "credential request principal id is invalid", err)
	}
	switch {
	case request.ExecutionID != "" && request.ServiceID != "":
		return fault.New(fault.InvalidInput, "credential request names both an execution and a service")
	case request.ExecutionID != "":
		if _, err := id.ParseExecutionID(string(request.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "credential request execution id is invalid", err)
		}
	case request.ServiceID != "":
		if _, err := service.ParseServiceID(string(request.ServiceID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "credential request service id is invalid", err)
		}
	default:
		return fault.New(fault.InvalidInput, "credential request names neither an execution nor a service")
	}
	return nil
}

// CredentialFact is the current committed credential state for one principal.
// Enabled is the current principal/service enablement observed at read time;
// it is current authority evidence for one commit, never durable permission.
type CredentialFact struct {
	Scope       tenancy.Scope     `json:"scope"`
	PrincipalID id.PrincipalID    `json:"principal_id"`
	ExecutionID id.ExecutionID    `json:"execution_id,omitempty"`
	ServiceID   service.ServiceID `json:"service_id,omitempty"`
	Enabled     bool              `json:"enabled"`
	Version     uint64            `json:"version"`
	Credential  Credential        `json:"credential"`
}

// TenantKey is one tenant's current encrypted derivation key. Every member is
// either ciphertext or a key-provider reference, never plaintext key or token
// material, and the whole value serializes as the JSON object stored in
// tenants.derivation_key jsonb.
//
// Ciphertext is the derivation key encrypted under the wrapped data key;
// WrappedKey is that data key wrapped by the key provider; WrapNonce is the
// wrap nonce; KeyRef names the provider key; KeyVersion is the provider key
// version. Version is the tenant derivation-key version and advances on every
// rotation.
type TenantKey struct {
	TenantID   id.TenantID `json:"tenant_id"`
	Version    uint64      `json:"version"`
	Ciphertext []byte      `json:"ciphertext"`
	WrappedKey []byte      `json:"wrapped_key"`
	WrapNonce  []byte      `json:"wrap_nonce"`
	KeyRef     string      `json:"key_ref"`
	KeyVersion uint64      `json:"key_version"`
}

// Validate checks the tenant identity, a non-zero key version, the encrypted
// members and the provider reference. No member may carry plaintext key or
// token material.
func (key TenantKey) Validate() error {
	if _, err := id.ParseTenantID(string(key.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "tenant key tenant id is invalid", err)
	}
	if key.Version == 0 {
		return fault.New(fault.InvalidInput, "tenant key version is zero")
	}
	if len(key.Ciphertext) == 0 {
		return fault.New(fault.InvalidInput, "tenant key ciphertext is empty")
	}
	if len(key.WrappedKey) == 0 {
		return fault.New(fault.InvalidInput, "tenant key wrapped key is empty")
	}
	if len(key.WrapNonce) == 0 {
		return fault.New(fault.InvalidInput, "tenant key wrap nonce is empty")
	}
	if key.KeyRef == "" {
		return fault.New(fault.InvalidInput, "tenant key reference is empty")
	}
	if key.KeyVersion == 0 {
		return fault.New(fault.InvalidInput, "tenant key provider version is zero")
	}
	return nil
}

// TenantKeyRequest asks for one tenant's current derivation key.
type TenantKeyRequest struct {
	TenantID id.TenantID `json:"tenant_id"`
}

// Validate checks the tenant identity.
func (request TenantKeyRequest) Validate() error {
	_, err := id.ParseTenantID(string(request.TenantID))
	return err
}

// TenantKeyWrite is the next tenant derivation key committed by a rotation.
// ExpectedVersion is the current tenant key version and Key.Version must be
// exactly the next version, so a stale or replayed rotation is refused instead
// of silently replacing a newer key.
type TenantKeyWrite struct {
	TenantID        id.TenantID `json:"tenant_id"`
	ExpectedVersion uint64      `json:"expected_version"`
	Key             TenantKey   `json:"key"`
}

// Validate checks the tenant, the current key version and the next key. A
// rotation must name an existing key version and advance it by exactly one.
func (write TenantKeyWrite) Validate() error {
	if _, err := id.ParseTenantID(string(write.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "tenant key write tenant id is invalid", err)
	}
	if write.TenantID != write.Key.TenantID {
		return fault.New(fault.InvalidInput, "tenant key write tenant ids do not match")
	}
	if write.ExpectedVersion == 0 {
		return fault.New(fault.InvalidInput, "tenant key write expected version is zero")
	}
	if write.Key.Version != write.ExpectedVersion+1 {
		return fault.New(fault.InvalidInput, "tenant key write does not advance the expected version")
	}
	return write.Key.Validate()
}

// MutationResult is the committed credential result. It deliberately omits any
// plaintext token: delivery is a post-commit effect with its own retry.
type MutationResult struct {
	State    State  `json:"state"`
	Version  uint64 `json:"version"`
	Replayed bool   `json:"replayed"`
}
