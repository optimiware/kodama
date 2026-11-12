package oidc

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// PrincipalIDFor derives the stable verified human principal id for one
// issuer/subject pair.
//
// The derivation is "oidc-" plus the lowercase hex encoding of the first 16
// SHA-256 bytes of issuer + "\x00" + subject. The NUL separator makes the
// encoding injective, so different issuer/subject pairs cannot collide by
// concatenation. An empty issuer or subject is refused as invalid input.
func PrincipalIDFor(issuer, subject string) (id.PrincipalID, error) {
	if issuer == "" {
		return "", fault.New(fault.InvalidInput, "OIDC issuer is empty")
	}
	if subject == "" {
		return "", fault.New(fault.InvalidInput, "OIDC subject is empty")
	}
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return id.PrincipalID("oidc-" + hex.EncodeToString(sum[:16])), nil
}
