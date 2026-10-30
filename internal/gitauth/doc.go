// Package gitauth is the authority of the internal Git service (ADR 0011,
// docs/specs/git-repositories.md GR-05, GR-06, GR-18; IT-17 to IT-19; seams S3,
// S5 and S8 of docs/implementation/internal-git-service.md): persistent, scoped
// Git grants and the opaque credentials that carry them, plus the verification
// the Git endpoint and the push authorizer use.
//
// A grant is the server's record of what a Git credential may do: actor, tenant,
// project, repositories, ref patterns, seed permission, an integration
// constraint, issue and expiry times, revocation and, for a seed, the one
// receive that consumed it. Its wire form is gitv1.GrantClaims; this package
// never redefines the contract's shapes, lifetimes, audiences or rules, it only
// applies them. The credential is an opaque random secret of at least 256 bits;
// only its digest is stored, and it is returned exactly once.
//
// Use cases (Service) coordinate small consumer-owned ports:
//
//   - a person exchanges a verified login for a human credential (IssueHuman) or
//     the single-use seed credential of an empty repository (IssueSeed); the
//     person's current project roles are checked at issuance and again at every
//     use (Verify), so a removed role takes effect on the next request;
//   - execution issues preparation, attempt and provisioning-seed grants
//     (IssueAttempt) and revokes them with the attempt (RevokeAttempt, or the
//     transaction-joined RevokeAttemptIn so a cancelled attempt can never push
//     after the cancellation commits);
//   - gates bind an integration grant to one approved gate decision
//     (IssueIntegration);
//   - the push authorizer consumes a seed grant for one receive (ConsumeSeed or
//     the transaction-joined ConsumeSeedIn) and reads grant claims by ID (Claims).
//
// Forwarding assertions (audience kodama-git-worker:<tenant>) and worker
// identities (audience kodama-git-control) are signed tokens with their own
// keys, issued and verified by the adapter in package jwt. Signing (the control
// plane: ForwardingSigner, WorkerIdentityIssuer) and verification (a worker, with
// public keys only: ForwardingVerifier) are separate ports; Assertions is their
// composite for a process that holds both. The three audiences are disjoint: an opaque Git credential is never a
// token, and neither token is ever accepted as a credential or as the other.
//
// This package imports no pgx, net/http or token library; adapters are the
// postgres, memory, jwt and http subpackages, wired by the composition root
// (T-132). Nothing here is served until that task registers it.
package gitauth
