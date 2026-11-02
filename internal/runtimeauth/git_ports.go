package runtimeauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// The consumer-owned ports of the runtime-issued Git grants (T-150, runtime v3).
// Two kinds of Git authority cannot exist at admission and are requested during
// the attempt: the single-use seed grant of a repository the attempt's run
// created, and the integration grant of an approved gate. The use cases are
// Service.SeedGitGrant and Service.IntegrationGitGrant; this module issues
// nothing itself. The adapters (runtimeauth/gitgrants) satisfy the ports over
// gitauth, the repository module and the gates module and are wired by the
// composition (T-132).

// GitCredential is the opaque secret of a Git grant, shown once. Every formatter
// and encoder prints a redaction, so it cannot reach a log or a stored document
// by accident; Reveal is the one deliberate exit (the response body).
type GitCredential struct{ value string }

// NewGitCredential wraps a credential.
func NewGitCredential(v string) GitCredential { return GitCredential{value: v} }

// Reveal returns the credential for delivery.
func (c GitCredential) Reveal() string { return c.value }

// IsZero reports an absent credential.
func (c GitCredential) IsZero() bool { return c.value == "" }

func (GitCredential) String() string               { return "[redacted]" }
func (GitCredential) GoString() string             { return "[redacted]" }
func (GitCredential) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }
func (GitCredential) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (c GitCredential) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[redacted]")) }
func (GitCredential) LogValue() slog.Value         { return slog.StringValue("[redacted]") }

// IssuedGitGrant is a grant the issuer recorded: its claims (not secret) and its
// credential (secret).
type IssuedGitGrant struct {
	Claims     gitv1.GrantClaims
	Credential GitCredential
}

// GitIssuer issues the runtime grants (provider: gitauth, T-136). Every method
// is idempotent on the attempt (seed) or on (gate, attempt) (integration): a
// repeat returns the same grant with a fresh credential that replaces the
// earlier one, until the grant is consumed, expired or revoked. A refusal is a
// *runtimev1.ContractError (gitauth's), which the service classifies as
// ErrRefused; an attempt that is no longer live is attempt_inactive.
type GitIssuer interface {
	// IssueSeed issues the seed grant of the attempt for the repository, whose
	// default branch the grant creates. notAfter bounds the grant (the run's
	// deadline); the issuer applies the contract's seed lifetime.
	IssueSeed(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, repo gitv1.Repository, notAfter time.Time) (IssuedGitGrant, error)
	// IssueIntegration issues the grant of exactly one constrained update of the
	// repository's default branch to the attempt.
	IssueIntegration(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, repositoryID string, c gitv1.IntegrationConstraint) (IssuedGitGrant, error)
	// RevokeAttempt revokes every grant of the attempt, idempotently, in its own
	// transaction. The service uses it only to close the issuance/cancellation
	// race (see Service.SeedGitGrant); a transition's own revocation joins its
	// transaction through the stores' GitGrants peer.
	RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error
}

// GitRepositories is this module's view of the repository module (T-127) and of
// the record of which run created a repository.
type GitRepositories interface {
	// Repository returns the repository of the scope's project. A foreign or
	// unknown repository is ErrNotFound, indistinguishable.
	Repository(ctx context.Context, scope domain.Scope, repositoryID string) (gitv1.Repository, error)
	// UnsettledPush reports whether the repository has any push record that is
	// authorized or unknown (gitv1.UnsettledPush).
	UnsettledPush(ctx context.Context, scope domain.Scope, repositoryID string) (bool, error)
	// CreatedByRun reports whether the run created the repository in the scope's
	// project: the creating run the repository module recorded in the reservation's
	// transaction (productmcp's Repositories.Create). An unknown repository is
	// false, not an error.
	CreatedByRun(ctx context.Context, scope domain.Scope, run domain.RunID, repositoryID string) (bool, error)
}

// ErrNoApproval is returned by IntegrationApprovals when no approved,
// still-applicable candidate exists for the gate and repository (the gates
// module's ErrNoApproval, or an unknown gate): no grant exists for it.
var ErrNoApproval = errors.New("runtimeauth: no approved candidate")

// ApprovalStale is returned by IntegrationApprovals when the default branch is no
// longer what the approval was given over: the pipeline recomputes the candidate
// (409 publication_conflict).
type ApprovalStale struct {
	// SupersedingCommit is the default branch's tip now.
	SupersedingCommit string
}

func (e *ApprovalStale) Error() string { return "runtimeauth: the approval's base moved" }

// ApprovalApplied is returned by IntegrationApprovals when the default branch
// already is the approved candidate: the update was applied and nothing is left
// to authorize.
type ApprovalApplied struct {
	Approval runtimev3.IntegrationApproval
}

func (e *ApprovalApplied) Error() string { return "runtimeauth: the approved candidate is integrated" }

// IntegrationApprovals is this module's view of the gates module (T-149). It
// answers from the gate and the repository, never from the request, and mints
// nothing. Its errors: ErrNoApproval (no approved candidate), *ApprovalStale,
// *ApprovalApplied, ErrNotFound (an absent or foreign gate, treated as no
// approval), or an outage (ErrUnavailable; never an approval).
type IntegrationApprovals interface {
	IntegrationApproval(ctx context.Context, scope domain.Scope, run domain.RunID, gateID, repositoryID string) (runtimev3.IntegrationApproval, error)
}

// IssuedGrant is the durable record of one runtime-issued Git grant of an attempt:
// who it was issued to and its claims (not secret). The result module loads these
// records, never the caller's word, to decide which commit outputs a runtime grant
// covers (runtimev3.CheckResult's issued grants).
type IssuedGrant struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	Claims    gitv1.GrantClaims
}

// Validate checks that the record is a seed or integration grant issued to the
// attempt's workload in the scope.
func (g IssuedGrant) Validate() error {
	if err := firstErr(g.Scope.Validate(), g.RunID.Validate(), g.AttemptID.Validate(), g.Claims.Validate()); err != nil {
		return fail(ErrInvalidRequest, "the issued grant is malformed")
	}
	c := g.Claims
	a := c.Actor
	if (c.Kind != gitv1.GrantSeed && c.Kind != gitv1.GrantIntegration) || len(c.Repositories) != 1 ||
		c.TenantID != string(g.Scope.TenantID) || c.ProjectID != string(g.Scope.ProjectID) || a.Type != gitv1.ActorWorkload ||
		a.AttemptID == nil || *a.AttemptID != string(g.AttemptID) || a.RunID == nil || *a.RunID != string(g.RunID) {
		return fail(ErrInvalidRequest, "the issued grant is not a runtime grant of this attempt")
	}
	return nil
}

// IssuedGrants is the persistence port of the records of runtime-issued grants
// (migration 014_attempt_git.sql).
type IssuedGrants interface {
	// RecordIssued stores the record idempotently by grant ID: the same record
	// again is a no-op, a different one under the same ID is ErrConflict.
	RecordIssued(ctx context.Context, g IssuedGrant) error
	// IssuedFor returns the claims of the attempt's recorded grants, ordered by
	// grant ID. An attempt with none has an empty list.
	IssuedFor(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]gitv1.GrantClaims, error)
}

// GitConfig is the optional Git half of Config. Without it every runtime Git
// grant operation is unavailable (there is no allow-all fallback).
type GitConfig struct {
	Issuer       GitIssuer
	Repositories GitRepositories
	Approvals    IntegrationApprovals
	Issued       IssuedGrants
}

func (c *GitConfig) validate() error {
	if c.Issuer == nil || c.Repositories == nil || c.Approvals == nil || c.Issued == nil {
		return errors.New("runtimeauth: a Git issuer, repositories, integration approvals and an issued-grant store are required (no allow-all fallback)")
	}
	return nil
}
