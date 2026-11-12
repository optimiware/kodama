package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// secretBytes is the entropy bound of every generated login secret. 32 bytes
// is 256 bits, well above the 128-bit state/nonce floor.
const secretBytes = 32

// PKCEChallenge returns the RFC 7636 S256 challenge of one verifier:
// base64url(sha256(ASCII(verifier))) without padding.
func PKCEChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// ValidatePKCEVerifier enforces the RFC 7636 verifier rule: 43 to 128
// characters of the unreserved alphabet [A-Za-z0-9-._~].
func ValidatePKCEVerifier(verifier string) error {
	if len(verifier) < 43 || len(verifier) > 128 {
		return fmt.Errorf("PKCE verifier must be 43..128 characters")
	}
	for index := 0; index < len(verifier); index++ {
		character := verifier[index]
		switch {
		case character >= 'A' && character <= 'Z':
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-', character == '.', character == '_', character == '~':
		default:
			return fmt.Errorf("PKCE verifier contains a character outside the unreserved alphabet")
		}
	}
	return nil
}

// NewPKCEVerifier draws 256 bits from the injected random source and encodes
// them as an unpadded base64url verifier.
func NewPKCEVerifier(random Random) (string, error) {
	secret, err := randomSecret(random)
	if err != nil {
		return "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	if err := ValidatePKCEVerifier(secret); err != nil {
		return "", err
	}
	return secret, nil
}

// NewState draws at least 128 bits of CSRF state from the injected source.
func NewState(random Random) (string, error) {
	secret, err := randomSecret(random)
	if err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return secret, nil
}

// NewNonce draws at least 128 bits of OIDC nonce from the injected source.
func NewNonce(random Random) (string, error) {
	secret, err := randomSecret(random)
	if err != nil {
		return "", fmt.Errorf("generate OIDC nonce: %w", err)
	}
	return secret, nil
}

// randomSecret reads exactly secretBytes once and encodes them unpadded
// base64url. It never retries on a short read: a partial entropy source is a
// failure, not a reason to loop.
func randomSecret(random Random) (string, error) {
	if random == nil {
		return "", errors.New("no random source is configured")
	}
	buffer := make([]byte, secretBytes)
	read, err := random.Read(buffer)
	if err != nil {
		return "", err
	}
	if read != len(buffer) {
		return "", errors.New("random source returned a short read")
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
