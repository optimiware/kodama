package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Verifier is the concrete identity.OIDCVerifier adapter.
//
// It is safe for concurrent use. The cached key set is replaced atomically and
// an unknown kid triggers at most one refetch per Verify.
type Verifier struct {
	config Config

	mu           sync.Mutex
	cache        *keySet
	lastForcedAt time.Time
	inflight     *jwksFlight
}

// NewVerifier validates the explicit configuration and builds a verifier.
//
// An invalid configuration is invalid_input and never yields a usable verifier.
// The declared identity.OIDCVerifier seam is the constructor result so an
// invalid configuration returns a genuinely nil seam value rather than a typed
// nil pointer.
func NewVerifier(config Config) (identity.OIDCVerifier, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.SubjectClaim == "" {
		config.SubjectClaim = defaultSubjectClaim
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Verifier{config: config}, nil
}

// Verify verifies one access token and returns the derived human principal.
//
// The token must carry the configured issuer, audience, subject and tenant
// claims and a valid expiry. The signature algorithm must be configured; "none"
// and every HS* value are refused. Malformed, oversized or unverifiable tokens
// are unauthenticated; provider failures are dependency_unavailable.
func (verifier *Verifier) Verify(ctx context.Context, request identity.OIDCRequest) (identity.Principal, error) {
	if err := request.Validate(); err != nil {
		return identity.Principal{}, err
	}
	if len(request.AccessToken) > verifier.config.MaxTokenBytes {
		return identity.Principal{}, unauthenticated("OIDC access token is too large")
	}

	token, err := verifier.parseToken(request)
	if err != nil {
		return identity.Principal{}, err
	}
	if err := verifier.verifySignature(ctx, token); err != nil {
		return identity.Principal{}, err
	}

	principalID, err := PrincipalIDFor(token.issuer, token.subject)
	if err != nil {
		return identity.Principal{}, unauthenticated("OIDC subject is invalid")
	}

	scope := tenancy.Scope{TenantID: id.TenantID(token.tenant)}
	if token.project != "" {
		scope.ProjectID = id.ProjectID(token.project)
	}
	return identity.Principal{
		ID:      principalID,
		Kind:    identity.KindHuman,
		Scope:   scope,
		Enabled: true,
	}, nil
}

// parsedToken is one structurally decoded and claim-validated access token.
type parsedToken struct {
	algorithm    string
	kid          string
	issuer       string
	subject      string
	tenant       string
	project      string
	signingInput []byte
	signature    []byte
}

type tokenHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

func (verifier *Verifier) parseToken(request identity.OIDCRequest) (*parsedToken, error) {
	parts := strings.Split(request.AccessToken, ".")
	if len(parts) != 3 {
		return nil, unauthenticated("OIDC access token is malformed")
	}
	headerBytes, err := decodeSegment(parts[0])
	if err != nil {
		return nil, unauthenticated("OIDC access token header is malformed")
	}
	payloadBytes, err := decodeSegment(parts[1])
	if err != nil {
		return nil, unauthenticated("OIDC access token payload is malformed")
	}
	signature, err := decodeSegment(parts[2])
	if err != nil {
		return nil, unauthenticated("OIDC access token signature is malformed")
	}

	var header tokenHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, unauthenticated("OIDC access token header is malformed")
	}
	if !verifier.algorithmAllowed(header.Algorithm) {
		return nil, unauthenticated("OIDC access token algorithm is not allowed")
	}
	if header.KeyID == "" {
		return nil, unauthenticated("OIDC access token has no key id")
	}

	claims, err := decodeClaims(payloadBytes)
	if err != nil {
		return nil, unauthenticated("OIDC access token claims are malformed")
	}

	issuer, ok := stringClaim(claims, "iss")
	if !ok || issuer != verifier.config.Issuer {
		return nil, unauthenticated("OIDC access token issuer does not match")
	}
	subject, ok := stringClaim(claims, verifier.config.SubjectClaim)
	if !ok {
		return nil, unauthenticated("OIDC access token subject is missing")
	}
	if !audienceMatches(claims["aud"], verifier.config.Audience) {
		return nil, unauthenticated("OIDC access token audience does not match")
	}
	tenant, ok := stringClaim(claims, verifier.config.TenantClaim)
	if !ok {
		return nil, unauthenticated("OIDC access token tenant is missing")
	}
	if _, err := id.ParseTenantID(tenant); err != nil {
		return nil, unauthenticated("OIDC access token tenant is invalid")
	}
	project := ""
	if verifier.config.ProjectClaim != "" {
		if _, present := claims[verifier.config.ProjectClaim]; present {
			project, ok = stringClaim(claims, verifier.config.ProjectClaim)
			if !ok {
				return nil, unauthenticated("OIDC access token project is invalid")
			}
			if _, err := id.ParseProjectID(project); err != nil {
				return nil, unauthenticated("OIDC access token project is invalid")
			}
		}
	}

	expiry, present, valid := numberClaim(claims, "exp")
	if !present {
		return nil, unauthenticated("OIDC access token expiry is missing")
	}
	if !valid {
		return nil, unauthenticated("OIDC access token expiry is malformed")
	}
	if !request.Now.Before(time.Unix(expiry, 0).Add(verifier.config.ClockSkew)) {
		return nil, unauthenticated("OIDC access token is expired")
	}

	if notBefore, present, valid := numberClaim(claims, "nbf"); present {
		if !valid {
			return nil, unauthenticated("OIDC access token not-before claim is malformed")
		}
		if request.Now.Add(verifier.config.ClockSkew).Before(time.Unix(notBefore, 0)) {
			return nil, unauthenticated("OIDC access token is not yet valid")
		}
	}
	if issuedAt, present, valid := numberClaim(claims, "iat"); present {
		if !valid {
			return nil, unauthenticated("OIDC access token issued-at claim is malformed")
		}
		if time.Unix(issuedAt, 0).After(request.Now.Add(verifier.config.ClockSkew)) {
			return nil, unauthenticated("OIDC access token issued-at claim is in the future")
		}
	}

	return &parsedToken{
		algorithm:    header.Algorithm,
		kid:          header.KeyID,
		issuer:       issuer,
		subject:      subject,
		tenant:       tenant,
		project:      project,
		signingInput: []byte(parts[0] + "." + parts[1]),
		signature:    signature,
	}, nil
}

func (verifier *Verifier) algorithmAllowed(algorithm string) bool {
	if algorithm == "" || algorithm == "none" || strings.HasPrefix(algorithm, "HS") {
		return false
	}
	return slices.Contains(verifier.config.Algorithms, algorithm)
}

func (verifier *Verifier) verifySignature(ctx context.Context, token *parsedToken) error {
	key, err := verifier.signingKey(ctx, token.kid)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(token.signingInput)
	switch token.algorithm {
	case AlgorithmRS256:
		if key.rsa == nil {
			return unauthenticated("OIDC access token key does not support the algorithm")
		}
		if err := rsa.VerifyPKCS1v15(key.rsa, crypto.SHA256, digest[:], token.signature); err != nil {
			return unauthenticated("OIDC access token signature is invalid")
		}
	case AlgorithmES256:
		if key.ec == nil {
			return unauthenticated("OIDC access token key does not support the algorithm")
		}
		if !verifyECDSASignature(key.ec, digest[:], token.signature) {
			return unauthenticated("OIDC access token signature is invalid")
		}
	default:
		return unauthenticated("OIDC access token algorithm is not allowed")
	}
	return nil
}

func (verifier *Verifier) signingKey(ctx context.Context, kid string) (verificationKey, error) {
	keys, err := verifier.keys(ctx, false)
	if err != nil {
		return verificationKey{}, err
	}
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	keys, err = verifier.keys(ctx, true)
	if err != nil {
		return verificationKey{}, err
	}
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return verificationKey{}, unauthenticated("OIDC access token key is unknown")
}

// verifyECDSASignature accepts the ASN.1 DER form used by ecdsa.SignASN1 and
// the fixed-width JOSE R||S form, so either encoding verifies correctly.
func verifyECDSASignature(key *ecdsa.PublicKey, digest, signature []byte) bool {
	if ecdsa.VerifyASN1(key, digest, signature) {
		return true
	}
	if len(signature) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	return ecdsa.Verify(key, digest, r, s)
}

func decodeSegment(value string) ([]byte, error) {
	if value == "" {
		return nil, errEmptySegment
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}

var errEmptySegment = fault.New(fault.Unauthenticated, "OIDC access token segment is empty")

func decodeClaims(payload []byte) (map[string]json.RawMessage, error) {
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	if claims == nil {
		return nil, errEmptySegment
	}
	return claims, nil
}

func stringClaim(claims map[string]json.RawMessage, name string) (string, bool) {
	value, present := claims[name]
	if !present {
		return "", false
	}
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return "", false
	}
	if decoded == "" {
		return "", false
	}
	return decoded, true
}

func audienceMatches(value json.RawMessage, want string) bool {
	if len(value) == 0 {
		return false
	}
	var single string
	if err := json.Unmarshal(value, &single); err == nil {
		return single == want
	}
	var many []string
	if err := json.Unmarshal(value, &many); err != nil {
		return false
	}
	return slices.Contains(many, want)
}

func numberClaim(claims map[string]json.RawMessage, name string) (int64, bool, bool) {
	value, present := claims[name]
	if !present {
		return 0, false, false
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return 0, true, false
	}
	number, ok := decoded.(json.Number)
	if !ok {
		return 0, true, false
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, true, false
	}
	return parsed, true, true
}

func unauthenticated(message string) error {
	return fault.New(fault.Unauthenticated, message)
}
