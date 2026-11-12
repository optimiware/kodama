// Derived credential encoding, verification and tenant derivation keys.
//
// The canonical MAC input and the token framing are declared values; this file
// only derives and verifies them. The identifier in a presented token is a
// lookup hint until the constant-time MAC comparison succeeds, and the encoded
// MAC is compared as one canonical fixed-size value so a truncated, padded or
// non-canonical encoding never authenticates.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// TenantKeyMaterial is the encrypted member set of one tenant derivation key.
// No member is plaintext key or token material.
type TenantKeyMaterial struct {
	Ciphertext []byte
	WrappedKey []byte
	WrapNonce  []byte
	KeyRef     string
	KeyVersion uint64
}

// DeriveExecutionToken returns the canonical kodex token for one execution
// credential. The tenant key must carry at least 256 bits.
func DeriveExecutionToken(key []byte, claims ExecutionClaims) (string, error) {
	input, err := ExecutionMACInput(claims)
	if err != nil {
		return "", err
	}
	mac, err := deriveMAC(key, input)
	if err != nil {
		return "", err
	}
	return ExecutionPrefix + string(claims.ExecutionID) + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

// DeriveServiceToken returns the canonical kodsvc token for one service
// credential. The tenant key must carry at least 256 bits.
func DeriveServiceToken(key []byte, claims ServiceClaims) (string, error) {
	input, err := ServiceMACInput(claims)
	if err != nil {
		return "", err
	}
	mac, err := deriveMAC(key, input)
	if err != nil {
		return "", err
	}
	return ServicePrefix + string(claims.ServiceID) + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

// VerifyExecutionToken checks a presented execution token against the supplied
// current claims and authoritative time. A wrong kind, tenant, principal, MAC or
// a reached expiry is refused without revealing token contents.
func VerifyExecutionToken(key []byte, presented string, claims ExecutionClaims, now time.Time) error {
	input, err := ExecutionMACInput(claims)
	if err != nil {
		return err
	}
	mac, err := deriveMAC(key, input)
	if err != nil {
		return err
	}
	return verifyPresented(presented, ExecutionPrefix, string(claims.ExecutionID), mac, claims.Expiry, now)
}

// VerifyServiceToken checks a presented service token against the supplied
// current claims and authoritative time.
func VerifyServiceToken(key []byte, presented string, claims ServiceClaims, now time.Time) error {
	input, err := ServiceMACInput(claims)
	if err != nil {
		return err
	}
	mac, err := deriveMAC(key, input)
	if err != nil {
		return err
	}
	return verifyPresented(presented, ServicePrefix, string(claims.ServiceID), mac, claims.Expiry, now)
}

func deriveMAC(key, input []byte) ([]byte, error) {
	if len(key) < MinNonceBytes {
		return nil, fault.New(fault.InvalidInput, "tenant derivation key is shorter than the minimum")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(input)
	return mac.Sum(nil), nil
}

func verifyPresented(presented, prefix, principal string, expectedMAC []byte, expiry, now time.Time) error {
	if now.IsZero() {
		return fault.New(fault.InvalidInput, "credential verification time is zero")
	}
	if !strings.HasPrefix(presented, prefix) {
		return fault.New(fault.Unauthenticated, "presented credential has the wrong kind")
	}
	segments := strings.Split(strings.TrimPrefix(presented, prefix), ".")
	if len(segments) != 2 {
		return fault.New(fault.Unauthenticated, "presented credential is malformed")
	}
	if segments[0] != principal {
		return fault.New(fault.Unauthenticated, "presented credential is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil || len(decoded) != sha256.Size {
		return fault.New(fault.Unauthenticated, "presented credential is malformed")
	}
	expectedSegment := base64.RawURLEncoding.EncodeToString(expectedMAC)
	if subtle.ConstantTimeCompare([]byte(segments[1]), []byte(expectedSegment)) != 1 {
		return fault.New(fault.Unauthenticated, "presented credential is invalid")
	}
	if !now.Before(expiry) {
		return fault.New(fault.Unauthenticated, "presented credential has expired")
	}
	return nil
}

// NewTenantKey builds one structured encrypted tenant derivation key at the
// supplied version. An incomplete or invalid member set is refused.
func NewTenantKey(tenantID id.TenantID, version uint64, material TenantKeyMaterial) (TenantKey, error) {
	key := TenantKey{
		TenantID:   tenantID,
		Version:    version,
		Ciphertext: cloneBytes(material.Ciphertext),
		WrappedKey: cloneBytes(material.WrappedKey),
		WrapNonce:  cloneBytes(material.WrapNonce),
		KeyRef:     material.KeyRef,
		KeyVersion: material.KeyVersion,
	}
	if err := key.Validate(); err != nil {
		return TenantKey{}, err
	}
	return key, nil
}

// NextTenantKey builds the next derivation-key rotation write. The next key
// version is exactly current.Version+1 and the expected version is the current
// one, so a stale or replayed rotation is refused.
func NextTenantKey(current TenantKey, material TenantKeyMaterial) (TenantKeyWrite, error) {
	if err := current.Validate(); err != nil {
		return TenantKeyWrite{}, err
	}
	next, err := NewTenantKey(current.TenantID, current.Version+1, material)
	if err != nil {
		return TenantKeyWrite{}, err
	}
	write := TenantKeyWrite{
		TenantID:        current.TenantID,
		ExpectedVersion: current.Version,
		Key:             next,
	}
	if err := write.Validate(); err != nil {
		return TenantKeyWrite{}, err
	}
	return write, nil
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
