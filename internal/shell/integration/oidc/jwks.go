package oidc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// p256CoordinateBytes is the width of one P-256 affine coordinate.
const p256CoordinateBytes = 32

var (
	errInvalidKey   = fault.New(fault.DependencyUnavailable, "OIDC JWKS key is invalid")
	errAmbiguousKey = fault.New(fault.DependencyUnavailable, "OIDC JWKS key id is ambiguous")
)

// verificationKey is one usable public signing key from a JWKS document.
type verificationKey struct {
	rsa *rsa.PublicKey
	ec  *ecdsa.PublicKey
}

// keySet is one immutable cached JWKS result.
type keySet struct {
	keys      map[string]verificationKey
	fetchedAt time.Time
}

// jwksFlight coalesces concurrent JWKS fetches into one HTTP request while
// respecting each waiting caller's context cancellation.
type jwksFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	force   bool
	keys    map[string]verificationKey
	err     error
}

// keys returns the current cached key set, fetching it when the cache is empty
// or older than CacheTTL. When force is true the cache is refreshed unless a
// forced refresh already succeeded within MinRefreshInterval, and concurrent
// callers share one in-flight fetch.
func (verifier *Verifier) keys(ctx context.Context, force bool) (map[string]verificationKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "OIDC identity provider is unavailable", err)
	}

	verifier.mu.Lock()
	now := verifier.config.Clock()
	if !force && verifier.cache != nil && now.Before(verifier.cache.fetchedAt.Add(verifier.config.CacheTTL)) {
		keys := verifier.cache.keys
		verifier.mu.Unlock()
		return keys, nil
	}
	if force && verifier.cache != nil && verifier.config.MinRefreshInterval > 0 &&
		!verifier.lastForcedAt.IsZero() && now.Sub(verifier.lastForcedAt) < verifier.config.MinRefreshInterval {
		keys := verifier.cache.keys
		verifier.mu.Unlock()
		return keys, nil
	}

	flight := verifier.inflight
	if flight != nil {
		if force {
			flight.force = true
		}
		flight.waiters++
		verifier.mu.Unlock()
	} else {
		flightCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &jwksFlight{
			done:    make(chan struct{}),
			cancel:  cancel,
			waiters: 1,
			force:   force,
		}
		verifier.inflight = flight
		verifier.mu.Unlock()

		go verifier.runJWKSFlight(flightCtx, flight)
	}

	select {
	case <-flight.done:
		return flight.keys, flight.err
	case <-ctx.Done():
		verifier.mu.Lock()
		flight.waiters--
		cancelFlight := flight.waiters == 0 && verifier.inflight == flight
		verifier.mu.Unlock()
		if cancelFlight {
			flight.cancel()
		}
		return nil, fault.Wrap(fault.DependencyUnavailable, "OIDC identity provider is unavailable", ctx.Err())
	}
}

func (verifier *Verifier) runJWKSFlight(ctx context.Context, flight *jwksFlight) {
	defer flight.cancel()
	keys, err := verifier.fetchJWKS(ctx)

	verifier.mu.Lock()
	if err == nil {
		fetchedAt := verifier.config.Clock()
		verifier.cache = &keySet{keys: keys, fetchedAt: fetchedAt}
		if flight.force {
			verifier.lastForcedAt = fetchedAt
		}
	}
	flight.keys = keys
	flight.err = err
	if verifier.inflight == flight {
		verifier.inflight = nil
	}
	close(flight.done)
	verifier.mu.Unlock()
}

// fetchJWKS performs one bounded JWKS request. Every provider failure is
// dependency_unavailable and never renders the provider body, URL or private
// transport cause.
func (verifier *Verifier) fetchJWKS(ctx context.Context) (map[string]verificationKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "OIDC identity provider is unavailable", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, verifier.config.JWKSURL, nil)
	if err != nil {
		return nil, fault.New(fault.DependencyUnavailable, "OIDC identity provider is unavailable")
	}
	request.Header.Set("Accept", "application/json")

	response, err := verifier.config.HTTPClient.Do(request)
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "OIDC identity provider is unavailable", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fault.New(fault.DependencyUnavailable, "OIDC identity provider returned an error status")
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, verifier.config.MaxJWKSBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "OIDC identity provider response could not be read", err)
	}
	if int64(len(body)) > verifier.config.MaxJWKSBytes {
		return nil, fault.New(fault.DependencyUnavailable, "OIDC identity provider response is too large")
	}

	keys, err := parseJWKS(body)
	if err != nil {
		return nil, fault.New(fault.DependencyUnavailable, "OIDC identity provider response is malformed")
	}
	return keys, nil
}

type jwksDocument struct {
	Keys []json.RawMessage `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS decodes a bounded JWKS document into usable signing keys.
//
// The document structure is strict: a malformed key entry makes the whole
// response malformed rather than silently shrinking the key set. Keys without
// a kid, and non-signature keys, cannot be selected and are ignored.
func parseJWKS(body []byte) (map[string]verificationKey, error) {
	var document jwksDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	keys := make(map[string]verificationKey, len(document.Keys))
	for _, raw := range document.Keys {
		var entry jwk
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, err
		}
		if entry.Use != "" && entry.Use != "sig" {
			continue
		}
		if entry.Kid == "" {
			continue
		}
		key, err := entry.verificationKey()
		if err != nil {
			return nil, err
		}
		if _, exists := keys[entry.Kid]; exists {
			return nil, errAmbiguousKey
		}
		keys[entry.Kid] = key
	}
	return keys, nil
}

func (entry jwk) verificationKey() (verificationKey, error) {
	switch entry.Kty {
	case "RSA":
		modulus, err := decodeBigInt(entry.N)
		if err != nil || modulus.Sign() <= 0 {
			return verificationKey{}, errInvalidKey
		}
		exponent, err := decodeExponent(entry.E)
		if err != nil {
			return verificationKey{}, err
		}
		return verificationKey{rsa: &rsa.PublicKey{N: modulus, E: exponent}}, nil
	case "EC":
		if entry.Crv != "P-256" {
			return verificationKey{}, errInvalidKey
		}
		x, err := decodeCoordinate(entry.X)
		if err != nil {
			return verificationKey{}, err
		}
		y, err := decodeCoordinate(entry.Y)
		if err != nil {
			return verificationKey{}, err
		}
		if len(x) > p256CoordinateBytes || len(y) > p256CoordinateBytes {
			return verificationKey{}, errInvalidKey
		}
		// The SEC 1 uncompressed form is 0x04 || X || Y with both coordinates
		// left padded to the field width.
		point := make([]byte, 1+2*p256CoordinateBytes)
		point[0] = 4
		copy(point[1+p256CoordinateBytes-len(x):], x)
		copy(point[len(point)-len(y):], y)
		// Parsing refuses a point off the curve, the point at infinity and a
		// coordinate that is not reduced modulo the field prime.
		key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
		if err != nil {
			return verificationKey{}, errInvalidKey
		}
		return verificationKey{ec: key}, nil
	default:
		return verificationKey{}, errInvalidKey
	}
}

func decodeBigInt(value string) (*big.Int, error) {
	decoded, err := decodeJWKSValue(value)
	if err != nil {
		return nil, err
	}
	if len(decoded) == 0 {
		return nil, errInvalidKey
	}
	return new(big.Int).SetBytes(decoded), nil
}

// decodeCoordinate decodes one big-endian curve coordinate and drops its
// leading zero octets, which carry no value: providers publish coordinates at
// the field width, in minimal form or with extra zero padding.
func decodeCoordinate(value string) ([]byte, error) {
	decoded, err := decodeJWKSValue(value)
	if err != nil {
		return nil, err
	}
	if len(decoded) == 0 {
		return nil, errInvalidKey
	}
	return bytes.TrimLeft(decoded, "\x00"), nil
}

func decodeExponent(value string) (int, error) {
	decoded, err := decodeJWKSValue(value)
	if err != nil {
		return 0, err
	}
	if len(decoded) == 0 || len(decoded) > 4 {
		return 0, errInvalidKey
	}
	exponent := 0
	for _, octet := range decoded {
		exponent = exponent<<8 | int(octet)
	}
	if exponent < 2 {
		return 0, errInvalidKey
	}
	return exponent, nil
}

func decodeJWKSValue(value string) ([]byte, error) {
	if value == "" {
		return nil, errInvalidKey
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}
