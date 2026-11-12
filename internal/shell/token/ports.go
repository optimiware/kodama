package token

import "context"

// MutationPort applies one complete credential lifecycle commit unit.
//
// Mutate must verify the expected principal version and every supplied current
// authority read inside the same transaction as the nonce/expiry replacement or
// clearing, receipt, audit and event publication intent. Delivery of a freshly
// issued token follows the commit as a retryable effect and is never part of
// the transaction.
type MutationPort interface {
	Mutate(context.Context, MutationUnit) (MutationResult, error)
}

// CredentialReader reads one principal's current credential pair and
// enablement at the authentication boundary. A fact is current evidence for
// one authentication, never durable permission.
type CredentialReader interface {
	ReadCredential(context.Context, CredentialRequest) (CredentialFact, error)
}

// TenantKeyReader reads one tenant's current encrypted derivation key. The
// adapter must never return plaintext key material.
type TenantKeyReader interface {
	ReadTenantKey(context.Context, TenantKeyRequest) (TenantKey, error)
}
