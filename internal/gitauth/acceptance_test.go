package gitauth_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	"github.com/optimiware/kodama/internal/gitauth/memory"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/memdb"
)

// The unit acceptance tests of T-136 run the shared scenarios on the memory
// adapter: no database. The PostgreSQL adapter runs the same scenarios in
// internal/gitauth/postgres (real SQL; skipped in the unit layer).

func Test_unit_gitauth_human_credential_exchange(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "human_credential_exchange")
}

func Test_unit_gitauth_current_role_on_use(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "current_role_on_use")
}

func Test_unit_gitauth_seed_grant_single_use(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "seed_grant_single_use")
}

func Test_unit_gitauth_seed_refused_non_empty(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "seed_refused_non_empty")
}

func Test_unit_gitauth_attempt_grant_scope(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "attempt_grant_scope")
}

func Test_unit_gitauth_integration_grant_binding(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "integration_grant_binding")
}

func Test_unit_gitauth_seed_issuance_vs_consumption(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "seed_issuance_vs_consumption")
}

func Test_unit_gitauth_revocation_transactional(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "revocation_transactional")
}

func Test_unit_gitauth_revoke_all_for_tenant(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "revoke_all_for_tenant")
}

func Test_unit_gitauth_active_grants_count(t *testing.T) {
	gitauthtest.Run(t, gitauthtest.OpenMem, "active_grants_count")
}

// Test_unit_gitauth_memory_contract runs the whole shared contract on the memory
// adapter.
func Test_unit_gitauth_memory_contract(t *testing.T) {
	gitauthtest.Contract(t, gitauthtest.OpenMem)
}

func Test_unit_gitauth_memory_is_development_only(t *testing.T) {
	e := gitauthtest.NewMemEnv(t)
	if d, ok := any(e.Mem).(interface{ DevelopmentOnly() bool }); !ok || !d.DevelopmentOnly() {
		t.Fatal("the memory adapter must be flagged development-only")
	}
	if _, err := memory.New(nil); err == nil {
		t.Fatal("a store needs a kernel")
	}
}

// Test_unit_gitauth_commit_failure_is_unknown_and_stores_nothing: a commit that
// fails after the use case succeeded is an unavailable outcome (never a
// credential) and discards the grant, so a retry starts clean.
func Test_unit_gitauth_commit_failure_is_unknown_and_stores_nothing(t *testing.T) {
	fail := true
	env := gitauthtest.NewMemEnv(t, memdb.WithCommitFault(func() error {
		if fail {
			return errors.New("commit lost")
		}
		return nil
	}))
	f := gitauthtest.New(t, func(testing.TB) gitauthtest.Env { return env })
	_, err := f.Svc.Exchange(context.Background(), gitauthtest.TenantA, gitauthtest.Engineer,
		gitv1.CredentialRequest{ProjectID: string(gitauthtest.ProjectA), RepositoryID: gitauthtest.RepoReady})
	if !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("an unknown commit outcome is unavailable: %v", err)
	}
	if n := env.GrantCount(t, gitauthtest.TenantA); n != 0 {
		t.Fatalf("the failed commit stored %d grants", n)
	}
	fail = false
	if _, err := f.Svc.Exchange(context.Background(), gitauthtest.TenantA, gitauthtest.Engineer,
		gitv1.CredentialRequest{ProjectID: string(gitauthtest.ProjectA), RepositoryID: gitauthtest.RepoReady}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func Test_unit_gitauth_secret_is_redacted_and_hashed(t *testing.T) {
	s, err := gitauth.GenerateSecret(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := s.Reveal()
	if len(raw) != 48 || !strings.HasPrefix(raw, "gitc_") {
		t.Fatalf("shape: %d bytes", len(raw))
	}
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))
	logger.Info("issued", "credential", s)
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s), sb.String()} {
		if strings.Contains(out, raw) {
			t.Fatalf("secret leaked: %s", out)
		}
	}
	if b, _ := s.MarshalJSON(); strings.Contains(string(b), raw) {
		t.Fatal("secret leaked in JSON")
	}
	if s.Digest().Hex() == raw || strings.Contains(s.Digest().Hex(), raw) || s.Digest().IsZero() {
		t.Fatal("the digest is a hash")
	}
	parsed, err := gitauth.ParseSecret(raw)
	if err != nil || parsed.Digest() != s.Digest() {
		t.Fatalf("parse: %v", err)
	}
	other, _ := gitauth.GenerateSecret(nil)
	if other.Digest() == s.Digest() || other.Reveal() == raw {
		t.Fatal("secrets are random")
	}
	for _, bad := range []string{"", raw[:47], raw + "A", "gitc_" + strings.Repeat("!", 43), strings.Repeat("x", 48), "adm_" + raw[5:]} {
		if _, err := gitauth.ParseSecret(bad); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Fatalf("ParseSecret(%q): %v", bad, err)
		}
	}
	if _, err := gitauth.GenerateSecret(strings.NewReader("short")); !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("no entropy is unavailable: %v", err)
	}
	// The credential fits the contract's wire syntax.
	if err := (gitv1.CredentialResponse{Username: "kodama", Password: raw, GrantID: "gg_1", ExpiresAt: "2026-10-30T10:00:00Z"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func Test_unit_gitauth_grant_entity_invariants(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	g := gitauth.Grant{Claims: gitv1.GrantClaims{
		GrantID: "gg_1", Audience: gitv1.GrantAudience, TenantID: "t", ProjectID: "p", Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: "u"}, Kind: gitv1.GrantSeed,
		Repositories: []gitv1.GrantRepository{{ID: "r", Read: true, Update: []string{"refs/heads/main"}, Seed: true}},
		IssuedAt:     "2026-10-30T10:00:00Z", ExpiresAt: "2026-10-30T10:10:00Z",
	}}
	if changed, err := g.Consume("rcv_1", now); !changed || err != nil {
		t.Fatalf("first consume: %v %v", changed, err)
	}
	if changed, err := g.Consume("rcv_1", now.Add(time.Hour)); changed || err != nil {
		t.Fatalf("the same receive is idempotent even later: %v %v", changed, err)
	}
	if _, err := g.Consume("rcv_2", now); !errors.Is(err, gitauth.ErrSeedConsumed) {
		t.Fatalf("another receive: %v", err)
	}
	if !g.Revoke(now.Add(-time.Hour)) || g.RevokedAt.Before(g.IssuedAt()) {
		t.Fatal("a revocation never precedes issuance")
	}
	if g.Revoke(now) {
		t.Fatal("revocation is idempotent")
	}
	fresh := gitauth.Grant{Claims: g.Claims}
	fresh.Revoke(now)
	if _, err := fresh.Consume("rcv_1", now); !errors.Is(err, gitauth.ErrGrantInactive) {
		t.Fatalf("a revoked seed: %v", err)
	}
	expired := gitauth.Grant{Claims: g.Claims}
	if _, err := expired.Consume("rcv_1", now.Add(10*time.Minute)); !errors.Is(err, gitauth.ErrGrantInactive) {
		t.Fatalf("an expired seed: %v", err)
	}
	human := gitauth.Grant{Claims: g.Claims}
	human.Claims.Kind = gitv1.GrantHuman
	if _, err := human.Consume("rcv_1", now); !gitauth.IsCode(err, domain.CodeInvalidRequest) {
		t.Fatalf("only a seed is consumed: %v", err)
	}
	cp := g.Clone()
	cp.Claims.Repositories[0].Update[0] = "x"
	*cp.RevokedAt = now.Add(time.Hour)
	if g.Claims.Repositories[0].Update[0] != "refs/heads/main" || g.RevokedAt.Equal(now.Add(time.Hour)) {
		t.Fatal("Clone shares state")
	}

	for _, bad := range []gitauth.Policy{{HumanTTL: 2 * time.Hour, SeedTTL: time.Minute, IntegrationTTL: time.Minute},
		{HumanTTL: time.Minute, SeedTTL: time.Hour, IntegrationTTL: time.Minute}, {HumanTTL: time.Minute, SeedTTL: time.Minute, IntegrationTTL: 48 * time.Hour},
		{HumanTTL: -1, SeedTTL: time.Minute, IntegrationTTL: time.Minute}} {
		if bad.Validate() == nil {
			t.Fatalf("policy %+v must be refused", bad)
		}
	}
	if err := gitauth.DefaultPolicy().Validate(); err != nil {
		t.Fatal(err)
	}
}

func Test_unit_gitauth_service_requires_ports(t *testing.T) {
	if _, err := gitauth.NewService(gitauth.Config{}); err == nil {
		t.Fatal("a service without ports must be refused")
	}
	f := gitauthtest.New(t, gitauthtest.OpenMem)
	authority, _ := identity.NewStandaloneAuthority(f.Roles)
	bad := gitauth.Config{Store: gitauthtest.NewMemEnv(t).Store(), Repositories: f.Repos, Pushes: f.Pushes, Authority: authority,
		Policy: gitauth.Policy{HumanTTL: 2 * time.Hour, SeedTTL: time.Minute, IntegrationTTL: time.Minute}}
	if _, err := gitauth.NewService(bad); err == nil {
		t.Fatal("an out-of-bounds policy must be refused")
	}
	// Without execution or gates the dependent use cases fail closed.
	bare := gitauth.Config{Store: gitauthtest.NewMemEnv(t).Store(), Repositories: f.Repos, Pushes: f.Pushes, Authority: authority, Clock: f.Clock.Now}
	svc, err := gitauth.NewService(bare)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.IssueIntegration(context.Background(), gitauthtest.ScopeA(), "att_1", gitauthtest.RepoReady,
		gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: strings.Repeat("2", 40), To: strings.Repeat("3", 40), GateID: "gate_1"})
	if !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("no gates: %v", err)
	}
	_, err = svc.IssueAttempt(context.Background(), gitauthtest.ScopeA(), gitauth.AttemptGrantRequest{Kind: gitv1.GrantAttempt, Actor: gitauthtest.Workload("att_1", "run_1"),
		Repositories: []gitv1.GrantRepository{{ID: gitauthtest.RepoReady, Read: true, Update: []string{}}}, ExpiresAt: f.Clock.Now().Add(time.Minute)})
	if !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("no attempts: %v", err)
	}
}
