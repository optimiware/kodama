package rtest

import (
	"context"
	"errors"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// IssuedClaims builds the claims of a runtime grant of an attempt: a seed grant
// of repository (create refs/heads/main) or an integration grant of the gate.
func IssuedClaims(kind gitv1.GrantKind, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, grantID, repository string) gitv1.GrantClaims {
	r, a := string(run), string(attempt)
	c := gitv1.GrantClaims{
		GrantID: grantID, Audience: gitv1.GrantAudience, TenantID: string(scope.TenantID), ProjectID: string(scope.ProjectID),
		Actor: gitv1.Actor{Type: gitv1.ActorWorkload, ID: a, RunID: &r, AttemptID: &a}, Kind: kind,
		IssuedAt: "2026-10-30T10:00:00Z", ExpiresAt: "2026-10-30T10:10:00Z",
	}
	switch kind {
	case gitv1.GrantSeed:
		c.Repositories = []gitv1.GrantRepository{{ID: repository, Read: true, Update: []string{"refs/heads/main"}, Seed: true}}
	default:
		c.ExpiresAt = "2026-10-30T10:30:00Z"
		c.Repositories = []gitv1.GrantRepository{{ID: repository, Read: true, Update: []string{"refs/heads/main"}, Integration: &gitv1.IntegrationConstraint{
			Ref: "refs/heads/main", From: "1111111111111111111111111111111111111111", To: "3333333333333333333333333333333333333333", GateID: "gate_07"}}}
	}
	return c
}

// OpenIssued opens one adapter of runtimeauth.IssuedGrants and the function that
// makes a scope's tenant and project known to it (the SQL adapter needs the
// project row). It skips the test when the adapter's service is not available.
type OpenIssued func(t testing.TB) (runtimeauth.IssuedGrants, func(domain.Scope))

// IssuedGrantsContract is the contract of the record of runtime-issued Git
// grants: every adapter passes it (memory on every unit run, PostgreSQL in the
// `test` environment).
//
//   - a record is stored once and idempotently by grant ID; the same record again
//     is a no-op and a different one under the same ID is ErrConflict;
//   - only a seed or integration grant of the attempt's own workload, in the
//     record's scope, is a record;
//   - records are read by attempt, ordered by grant ID, and never across tenants,
//     projects or attempts.
func IssuedGrantsContract(t *testing.T, open OpenIssued) {
	ctx := context.Background()
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	other := domain.Scope{TenantID: "t_globex", ProjectID: "p_inventory"}
	sibling := domain.Scope{TenantID: "t_acme", ProjectID: "p_ledger"}

	t.Run("idempotent by grant id", func(t *testing.T) {
		s, seed := open(t)
		seed(scope)
		g := runtimeauth.IssuedGrant{Scope: scope, RunID: "run_01", AttemptID: "att_01", Claims: IssuedClaims(gitv1.GrantSeed, scope, "run_01", "att_01", "gg_seed01", "repo_01")}
		for i := 0; i < 2; i++ {
			if err := s.RecordIssued(ctx, g); err != nil {
				t.Fatalf("record %d: %v", i, err)
			}
		}
		got, err := s.IssuedFor(ctx, scope, "att_01")
		if err != nil || len(got) != 1 || got[0].GrantID != "gg_seed01" {
			t.Fatalf("read: %+v %v", got, err)
		}
		changed := g
		changed.Claims = IssuedClaims(gitv1.GrantSeed, scope, "run_01", "att_01", "gg_seed01", "repo_02")
		if err := s.RecordIssued(ctx, changed); !errors.Is(err, runtimeauth.ErrConflict) {
			t.Fatalf("a different record under the same ID: %v", err)
		}
		if got, _ := s.IssuedFor(ctx, scope, "att_01"); len(got) != 1 || got[0].Repositories[0].ID != "repo_01" {
			t.Fatalf("the conflicting record replaced the first: %+v", got)
		}
	})

	t.Run("only runtime grants of the attempt", func(t *testing.T) {
		s, seed := open(t)
		seed(scope)
		attempt := IssuedClaims(gitv1.GrantAttempt, scope, "run_01", "att_01", "gg_att01", "repo_01")
		attempt.Repositories[0].Seed = false
		attempt.Repositories[0].Integration = nil
		attempt.Repositories[0].Update = []string{"refs/heads/kodama/runs/run_01/*"}
		attempt.ExpiresAt = "2026-10-30T11:00:00Z"
		for name, g := range map[string]runtimeauth.IssuedGrant{
			"an attempt grant": {Scope: scope, RunID: "run_01", AttemptID: "att_01", Claims: attempt},
			"another attempt":  {Scope: scope, RunID: "run_01", AttemptID: "att_02", Claims: IssuedClaims(gitv1.GrantSeed, scope, "run_01", "att_01", "gg_s", "repo_01")},
			"another run":      {Scope: scope, RunID: "run_02", AttemptID: "att_01", Claims: IssuedClaims(gitv1.GrantSeed, scope, "run_01", "att_01", "gg_s", "repo_01")},
			"another project":  {Scope: scope, RunID: "run_01", AttemptID: "att_01", Claims: IssuedClaims(gitv1.GrantSeed, sibling, "run_01", "att_01", "gg_s", "repo_01")},
			"malformed claims": {Scope: scope, RunID: "run_01", AttemptID: "att_01", Claims: gitv1.GrantClaims{}},
			"a zero attempt":   {Scope: scope, RunID: "run_01", Claims: IssuedClaims(gitv1.GrantSeed, scope, "run_01", "att_01", "gg_s", "repo_01")},
			"a human's grant id": {Scope: scope, RunID: "run_01", AttemptID: "att_01", Claims: func() gitv1.GrantClaims {
				c := IssuedClaims(gitv1.GrantSeed, scope, "run_01", "att_01", "gg_s", "repo_01")
				c.Actor = gitv1.Actor{Type: gitv1.ActorUser, ID: "u_1"}
				return c
			}()},
		} {
			if err := s.RecordIssued(ctx, g); err == nil {
				t.Errorf("%s was recorded", name)
			}
		}
		if got, _ := s.IssuedFor(ctx, scope, "att_01"); len(got) != 0 {
			t.Fatalf("a refused record was stored: %+v", got)
		}
	})

	t.Run("read by attempt, scope and tenant", func(t *testing.T) {
		s, seed := open(t)
		seed(scope)
		seed(other)
		seed(sibling)
		for _, g := range []runtimeauth.IssuedGrant{
			{Scope: scope, RunID: "run_01", AttemptID: "att_01", Claims: IssuedClaims(gitv1.GrantIntegration, scope, "run_01", "att_01", "gg_b", "repo_01")},
			{Scope: scope, RunID: "run_01", AttemptID: "att_01", Claims: IssuedClaims(gitv1.GrantSeed, scope, "run_01", "att_01", "gg_a", "repo_02")},
			{Scope: scope, RunID: "run_01", AttemptID: "att_02", Claims: IssuedClaims(gitv1.GrantSeed, scope, "run_01", "att_02", "gg_c", "repo_03")},
			{Scope: other, RunID: "run_01", AttemptID: "att_01", Claims: IssuedClaims(gitv1.GrantSeed, other, "run_01", "att_01", "gg_a", "repo_01")},
			{Scope: sibling, RunID: "run_01", AttemptID: "att_01", Claims: IssuedClaims(gitv1.GrantSeed, sibling, "run_01", "att_01", "gg_d", "repo_01")},
		} {
			if err := s.RecordIssued(ctx, g); err != nil {
				t.Fatalf("record %s: %v", g.Claims.GrantID, err)
			}
		}
		got, err := s.IssuedFor(ctx, scope, "att_01")
		if err != nil || len(got) != 2 || got[0].GrantID != "gg_a" || got[1].GrantID != "gg_b" {
			t.Fatalf("the attempt's records, ordered: %+v %v", got, err)
		}
		if got, _ := s.IssuedFor(ctx, other, "att_01"); len(got) != 1 || got[0].TenantID != "t_globex" {
			t.Fatalf("the other tenant's attempt of the same ID: %+v", got)
		}
		if got, _ := s.IssuedFor(ctx, sibling, "att_02"); len(got) != 0 {
			t.Fatalf("another project's read crossed: %+v", got)
		}
		if _, err := s.IssuedFor(ctx, domain.Scope{}, "att_01"); err == nil {
			t.Fatal("an empty scope was read")
		}
	})
}
