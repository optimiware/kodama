// Package oidc is the access-token verification adapter of the identity module.
// It uses github.com/coreos/go-oidc/v3 for discovery and claim verification
// (issuer, audience, expiry, signing algorithm) and go-jose for signatures, with
// a JWKS cache that distinguishes an unreachable issuer from a bad credential.
//
// Only API access tokens are accepted: the configured audience must be the
// factory API audience, which an OIDC client's ID token does not carry, and
// tokens bearing ID-token-only claims are rejected even if their audience was
// misconfigured. Signature, issuer, audience and expiry checks are never
// disabled. Nothing here reads membership or role claims: persona and token role
// claims establish no authority.
package oidc
