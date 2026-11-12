package git_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/api"
	git "github.com/optimiware/kodama/internal/userland/git"
)

// TestReadAuthorizationLocalRules pins the repository-level smart-HTTP read
// decision for every principal kind: users with scoped RBAC roles, service
// accounts and short-lived pipeline service accounts are evaluated against the
// repository's ordered repository-wide rules. R, RW and RW+ cover a read; a
// matching deny or no repository-wide match refuses; an approval option gates
// updates only. The actor is verified separately with the repository-level
// git.read operation, never by this local seam.
func TestReadAuthorizationLocalRules(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name       string
		repository string
		identity   git.Identity
		want       git.Decision
	}{
		{"viewer reads through @all", "shop/web", humanIdentity("viewer"), git.DecisionAllow},
		{"engineer reads through @all", "shop/web", humanIdentity("engineer"), git.DecisionAllow},
		{"execution reads through @all", "shop/web", executionIdentity(), git.DecisionAllow},
		{"operator reads deploy", "shop/deploy", humanIdentity("operator"), git.DecisionAllow},
		{"engineer cannot read deploy", "shop/deploy", humanIdentity("engineer"), git.DecisionDeny},
		{"a ref-scoped service rule does not cover the repository", "shop/guarded", serviceIdentity("ci-pipeline"), git.DecisionDeny},
		{"unlisted service account refuses", "shop/guarded", serviceIdentity("other-svc"), git.DecisionDeny},
		{"a ref-scoped deny and ref-prefix allow do not cover the repository", "shop/guarded", humanIdentity("engineer"), git.DecisionDeny},
		{"viewer has no rule on guarded", "shop/guarded", humanIdentity("viewer"), git.DecisionDeny},
		{"admin rule is ref-less", "admin", humanIdentity("admin"), git.DecisionAllow},
		{"non-admin cannot read admin", "admin", humanIdentity("engineer"), git.DecisionDeny},
	}
	for _, testCase := range cases {
		decision, err := f.service.AuthorizeRead(context.Background(), testCase.repository, testCase.identity)
		if err != nil {
			t.Errorf("%s: AuthorizeRead returned error: %v", testCase.name, err)
			continue
		}
		if decision != testCase.want {
			t.Errorf("%s: decision = %q; want %q", testCase.name, decision, testCase.want)
		}
	}

	// The local read seam never calls service verification itself: the actor is
	// verified once with the repository-level git.read operation before the
	// seam, and there is no `git.ref.read` operation or per-ref update approval
	// on a read.
	if calls := f.verifier.verifyRequests(); len(calls) != 0 {
		t.Fatalf("local read seam called verification %d times: %+v", len(calls), calls)
	}
	if calls := f.verifier.reportRequests(); len(calls) != 0 {
		t.Fatalf("read path reported %d times", len(calls))
	}
}

// TestReadAuthorizationProtectedBranchBoundary pins that a protected branch is
// readable under a covering rule while writes stay restricted: the read
// decision is not an update decision and the approval option does not block it.
func TestReadAuthorizationProtectedBranchBoundary(t *testing.T) {
	f := newFixture(t)
	policy, ok := f.policies.config.Policy("shop/web")
	if !ok {
		t.Fatal("fixture policy shop/web is missing")
	}
	if role, ok := policy.ApprovalFor(testRef); !ok || role != "reviewer" {
		t.Fatalf("fixture approval = %q,%v; want reviewer,true", role, ok)
	}
	for _, identity := range []git.Identity{executionIdentity(), humanIdentity("viewer")} {
		decision, err := f.service.AuthorizeRead(context.Background(), "shop/web", identity)
		if err != nil || decision != git.DecisionAllow {
			t.Fatalf("protected-branch read for %+v = %q,%v; want allow", identity, decision, err)
		}
	}
}

// TestReadAuthorizationRepositoryLevelDecision pins that read authorization
// is repository-level, so a ref-scoped deny followed by a ref-less read rule
// permits the repository read, while a ref-prefix-only rule never covers the
// whole repository.
func TestReadAuthorizationRepositoryLevelDecision(t *testing.T) {
	policy, err := git.ParseConfig([]byte(`
repo shop/private
    -    refs/heads/private        = @viewer
    R                              = @viewer

repo shop/scoped
    RW   refs/heads/               = @engineer

repo shop/blocked
    -                              = @viewer
    R                              = @viewer
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cases := []struct {
		name       string
		repository string
		identity   git.Identity
		want       git.Decision
	}{
		{"ref-scoped deny does not hide the repository", "shop/private", humanIdentity("viewer"), git.DecisionAllow},
		{"a non-matching role refuses", "shop/private", humanIdentity("reviewer"), git.DecisionDeny},
		{"a service identity never matches a human role", "shop/private", serviceIdentity("shop-svc"), git.DecisionDeny},
		{"a ref-prefix-only rule does not cover the repository", "shop/scoped", humanIdentity("engineer"), git.DecisionDeny},
		{"a ref-less deny refuses the repository", "shop/blocked", humanIdentity("viewer"), git.DecisionDeny},
	}
	for _, testCase := range cases {
		target, ok := policy.Policy(testCase.repository)
		if !ok {
			t.Fatalf("%s: Policy(%q) is missing", testCase.name, testCase.repository)
		}
		decision, err := target.AuthorizeRead(git.ReadRequest{Identity: testCase.identity})
		if err != nil {
			t.Errorf("%s: AuthorizeRead returned error: %v", testCase.name, err)
			continue
		}
		if decision != testCase.want {
			t.Errorf("%s: decision = %q; want %q", testCase.name, decision, testCase.want)
		}
	}
}

// TestVerifyReadActorUsesGitReadAndDropsServiceRoles pins the read path:
// the smart-HTTP read actor is verified through the public verification client
// with the repository-level `git.read` operation (empty subject, no approval),
// and a verified service carrying its scoped RBAC roles is mapped by service id
// alone. Those roles never grant a human-role rule match.
func TestVerifyReadActorUsesGitReadAndDropsServiceRoles(t *testing.T) {
	f := newFixtureWithConfig(t, "repo shop/svc\n    R = @engineer\n    RW refs/heads/svc = shop-svc\n")
	f.verifier.script("svc-token", verificationScript{
		principal:      api.VerifiedPrincipal{ID: "shop-svc", Kind: "service"},
		roles:          []string{"engineer"},
		verificationID: testVerificationID,
	})

	readVerifier, err := git.NewReadVerifier(f.verifier, func(repository string) (string, error) {
		if repository != "shop/svc" {
			return "", fmt.Errorf("unexpected repository %q", repository)
		}
		return testResourceID, nil
	})
	if err != nil {
		t.Fatalf("NewReadVerifier: %v", err)
	}
	identity, err := readVerifier.VerifyReadActor(context.Background(), "shop/svc", "svc-token")
	if err != nil {
		t.Fatalf("VerifyReadActor: %v", err)
	}
	if !reflect.DeepEqual(identity, git.Identity{ServiceID: "shop-svc"}) {
		t.Fatalf("verified identity = %+v; want the service id without roles", identity)
	}
	calls := f.verifier.verifyRequests()
	if len(calls) != 1 {
		t.Fatalf("verification calls = %d; want 1", len(calls))
	}
	request := calls[0]
	if request.Operation != string(api.ResourceOperationGitRead) {
		t.Fatalf("verification operation = %q; want git.read", request.Operation)
	}
	if request.ResourceID != testResourceID || request.ActorCredential != "svc-token" {
		t.Fatalf("verification request = %+v", request)
	}
	if len(request.Subject.Fields) != 0 || request.Approval != "" {
		t.Fatalf("git.read verification carried subject/approval: %+v", request)
	}
	if _, err := readVerifier.VerifyReadActor(context.Background(), "shop/missing", "svc-token"); err == nil {
		t.Fatal("VerifyReadActor accepted a repository with no resource identity")
	}
	if _, err := git.NewReadVerifier(nil, func(string) (string, error) { return testResourceID, nil }); err == nil {
		t.Fatal("NewReadVerifier accepted a nil verification client")
	}
	if _, err := git.NewReadVerifier(f.verifier, nil); err == nil {
		t.Fatal("NewReadVerifier accepted a nil resource resolver")
	}

	policy, ok := f.policies.config.Policy("shop/svc")
	if !ok {
		t.Fatal("fixture policy shop/svc is missing")
	}
	// The service id rule matches the mapped identity.
	decision, err := policy.Authorize(git.AccessRequest{Identity: identity, Ref: "refs/heads/svc", Update: git.UpdateCreate})
	if err != nil || decision != git.DecisionAllow {
		t.Fatalf("service-id write rule = %q,%v; want allow", decision, err)
	}
	// The verified human role does not leak into the service identity: the
	// ref-less @engineer read rule does not cover the service.
	decision, err = policy.AuthorizeRead(git.ReadRequest{Identity: identity})
	if err != nil || decision != git.DecisionDeny {
		t.Fatalf("service repository read through a human role = %q,%v; want deny", decision, err)
	}

	// A read verification is verification-only: it never reports an effect, and
	// a failed verification is refused rather than mapped to an identity.
	if calls := f.verifier.reportRequests(); len(calls) != 0 {
		t.Fatalf("read verification reported %d resource effects; want none", len(calls))
	}
	f.verifier.fail(errors.New("verification unavailable"))
	if _, err := readVerifier.VerifyReadActor(context.Background(), "shop/svc", "svc-token"); err == nil {
		t.Fatal("VerifyReadActor accepted a failed verification")
	}
}

// TestReadAuthorizationReadsCurrentPolicy pins that a rule change applies to
// the next read: the service reads the current tenant config for every attempt
// instead of caching the parsed rules.
func TestReadAuthorizationReadsCurrentPolicy(t *testing.T) {
	f := newFixtureWithConfig(t, "repo shop/web\n    R = @viewer\n")
	viewer := humanIdentity("viewer")
	engineer := humanIdentity("engineer")

	decision, err := f.service.AuthorizeRead(context.Background(), "shop/web", viewer)
	if err != nil || decision != git.DecisionAllow {
		t.Fatalf("initial read = %q,%v; want allow", decision, err)
	}

	// An accepted admin push replaced the rules: the repository-level read now
	// belongs to the engineer, not the viewer.
	f.policies.set(mustConfig(t, "repo shop/web\n    R = @engineer\n"))
	decision, err = f.service.AuthorizeRead(context.Background(), "shop/web", viewer)
	if err != nil {
		t.Fatalf("read after rule change: %v", err)
	}
	if decision != git.DecisionDeny {
		t.Fatalf("read after rule change = %q; want deny", decision)
	}
	decision, err = f.service.AuthorizeRead(context.Background(), "shop/web", engineer)
	if err != nil || decision != git.DecisionAllow {
		t.Fatalf("read by newly allowed role = %q,%v; want allow", decision, err)
	}
	if calls := f.policies.callCount(); calls < 3 {
		t.Fatalf("policy reads = %d; want one per attempt", calls)
	}
}

// TestReadAuthorizationRefusals pins fail-closed read refusals: an unknown
// repository, an unreadable policy and a malformed identity all refuse without
// a decision.
func TestReadAuthorizationRefusals(t *testing.T) {
	f := newFixture(t)
	viewer := humanIdentity("viewer")

	if decision, err := f.service.AuthorizeRead(context.Background(), "shop/missing", viewer); !errors.Is(err, git.ErrRepositoryUnknown) {
		t.Fatalf("unknown repository = %q,%v; want ErrRepositoryUnknown", decision, err)
	}
	if decision, err := f.service.AuthorizeRead(context.Background(), "../escape", viewer); !errors.Is(err, git.ErrInvalidRepository) {
		t.Fatalf("escaped repository = %q,%v; want ErrInvalidRepository", decision, err)
	}
	if _, err := f.service.AuthorizeRead(context.Background(), "shop/web", git.Identity{}); err == nil {
		t.Fatal("AuthorizeRead accepted an empty identity")
	}

	f.policies.mu.Lock()
	f.policies.err = errors.New("tenant config unavailable")
	f.policies.mu.Unlock()
	if _, err := f.service.AuthorizeRead(context.Background(), "shop/web", viewer); err == nil {
		t.Fatal("AuthorizeRead ignored a policy-source failure")
	}
}
