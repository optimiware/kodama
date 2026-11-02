package gitauth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	gitjwt "github.com/optimiware/kodama/internal/gitauth/jwt"
)

// Test_unit_gitauth_audiences_disjoint: the three Git audiences never validate as
// one another (IT-18): an opaque credential (kodama-git), a forwarding assertion
// (kodama-git-worker:<tenant>) and a worker identity (kodama-git-control) are
// each accepted only where their own kind is, by their own keys.
func Test_unit_gitauth_audiences_disjoint(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	ring := func() *gitjwt.KeyRing {
		key, err := gitjwt.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		r, err := gitjwt.NewKeyRing(1, key)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	signer, err := gitjwt.NewSigner(gitjwt.SignerConfig{Issuer: "https://git.t136.invalid", Forwarding: ring(), Identity: ring(), Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	var tokens gitauth.Assertions = signer.Tokens()

	f := gitauthtest.New(t, gitauthtest.OpenMem)
	tenant := gitauthtest.TenantA
	cred := f.HumanCredential(gitauthtest.Engineer, gitauthtest.RepoReady)

	assertion := gitv1.ForwardingAssertion{
		Audience: gitv1.WorkerAudience(string(tenant)), TenantID: string(tenant), RepositoryID: gitauthtest.RepoReady, GrantID: "gg_1", ReceivePack: true,
		IssuedAt: "2026-10-30T10:00:00Z", ExpiresAt: "2026-10-30T10:04:00Z", RequestID: "req_1",
	}
	identity := gitv1.WorkerIdentity{Audience: gitv1.ControlAudience, TenantID: string(tenant), WorkerInstance: "worker_1", IssuedAt: "2026-10-30T10:00:00Z", ExpiresAt: "2026-10-30T10:30:00Z"}
	fwd, err := tokens.SignForwarding(ctx, assertion)
	if err != nil {
		t.Fatal(err)
	}
	idn, err := tokens.SignWorkerIdentity(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}

	// The constants are three different audiences, and the per-tenant one differs per tenant.
	if gitv1.GrantAudience == gitv1.ControlAudience || strings.HasPrefix(gitv1.GrantAudience, gitv1.WorkerAudiencePrefix) || strings.HasPrefix(gitv1.ControlAudience, gitv1.WorkerAudiencePrefix) ||
		gitv1.WorkerAudience("a") == gitv1.WorkerAudience("b") {
		t.Fatal("audiences overlap")
	}

	// A signed token is never a Git credential.
	for name, token := range map[string]string{"forwarding assertion": fwd, "worker identity": idn} {
		if _, err := f.Svc.Verify(ctx, tenant, token); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
			t.Errorf("%s accepted as a Git credential: %v", name, err)
		}
	}
	// An opaque credential is never a token.
	if _, err := tokens.VerifyForwarding(ctx, cred, tenant, now); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Errorf("a Git credential accepted as a forwarding assertion: %v", err)
	}
	if _, err := tokens.VerifyWorkerIdentity(ctx, cred, tenant, now); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Errorf("a Git credential accepted as a worker identity: %v", err)
	}
	// The two tokens are not each other.
	if _, err := tokens.VerifyForwarding(ctx, idn, tenant, now); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Errorf("a worker identity accepted as a forwarding assertion: %v", err)
	}
	if _, err := tokens.VerifyWorkerIdentity(ctx, fwd, tenant, now); !gitauth.IsCode(err, domain.CodeInvalidCredential) {
		t.Errorf("a forwarding assertion accepted as a worker identity: %v", err)
	}
	// Each verifies where it belongs.
	if _, err := tokens.VerifyForwarding(ctx, fwd, tenant, now); err != nil {
		t.Errorf("forwarding: %v", err)
	}
	if _, err := tokens.VerifyWorkerIdentity(ctx, idn, tenant, now); err != nil {
		t.Errorf("identity: %v", err)
	}
	if _, err := f.Svc.Verify(ctx, tenant, cred); err != nil {
		t.Errorf("credential: %v", err)
	}

	// The contract's claim sets enforce their own audience.
	claims := f.MustVerify(tenant, cred)
	for name, a := range map[string]string{"control": gitv1.ControlAudience, "worker": gitv1.WorkerAudience(string(tenant)), "none": ""} {
		c := claims
		c.Audience = a
		if c.Validate() == nil {
			t.Errorf("grant claims with the %s audience validate", name)
		}
	}
	for name, a := range map[string]string{"grant": gitv1.GrantAudience, "control": gitv1.ControlAudience, "other tenant": gitv1.WorkerAudience("t_other")} {
		x := assertion
		x.Audience = a
		if x.Validate() == nil {
			t.Errorf("a forwarding assertion with the %s audience validates", name)
		}
	}
	for name, a := range map[string]string{"grant": gitv1.GrantAudience, "worker": gitv1.WorkerAudience(string(tenant))} {
		x := identity
		x.Audience = a
		if x.Validate() == nil {
			t.Errorf("a worker identity with the %s audience validates", name)
		}
	}
}
