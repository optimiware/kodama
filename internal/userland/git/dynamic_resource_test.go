package git_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	git "github.com/optimiware/kodama/internal/userland/git"
)

func TestParseConfigOptionResourceValidation(t *testing.T) {
	parsed, err := git.ParseConfig([]byte(`
repo admin
    RW = @admin

repo shop/web
    option resource = shop_web-res1
    R = @all
    RW+ refs/heads/work/ = @engineer @execution
    RW refs/heads/main = @execution
    option approve.refs/heads/main = reviewer
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	admin, ok := parsed.Policy("admin")
	if !ok || admin.ResourceID != "" {
		t.Fatalf("admin policy = %+v, %v; want empty ResourceID", admin, ok)
	}
	web, ok := parsed.Policy("shop/web")
	if !ok || web.ResourceID != "shop_web-res1" {
		t.Fatalf("shop/web ResourceID = %q (ok=%v); want shop_web-res1", web.ResourceID, ok)
	}
	if role, guarded := web.ApprovalFor("refs/heads/main"); !guarded || role != "reviewer" {
		t.Fatalf("shop/web ApprovalFor(main) = %q, %v; want reviewer, true", role, guarded)
	}

	refusals := []struct {
		name string
		conf string
	}{
		{"before repo", "option resource = res-1\nrepo app\n    R = @all\n"},
		{"duplicate in same stanza", "repo app\n    option resource = res-1\n    option resource = res-1\n    R = @all\n"},
		{"duplicate different value", "repo app\n    option resource = res-1\n    option resource = res-2\n    R = @all\n"},
		{"empty value", "repo app\n    option resource =\n    R = @all\n"},
		{"multiple values", "repo app\n    option resource = res-1 res-2\n    R = @all\n"},
		{"slash in resource id", "repo app\n    option resource = shop/web\n    R = @all\n"},
		{"dot in resource id", "repo app\n    option resource = res.1\n    R = @all\n"},
		{"single dot", "repo app\n    option resource = .\n    R = @all\n"},
		{"double dot", "repo app\n    option resource = ..\n    R = @all\n"},
		{"overlong resource id", "repo app\n    option resource = " + strings.Repeat("a", 129) + "\n    R = @all\n"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := git.ParseConfig([]byte(tc.conf)); err == nil {
				t.Fatalf("ParseConfig accepted invalid option resource: %q", tc.conf)
			}
		})
	}
}

func TestServiceResolvesDynamicAndStaticResourceIDsAndRefusesMismatch(t *testing.T) {
	storage, err := git.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	log := &callLog{}
	executor := newFakeExecutor(log)
	policies := &fakePolicySource{config: mustConfig(t, `
repo admin
    RW = @admin

repo static/only
    R = @all

repo static/agreed
    option resource = res-agreed
    R = @all

repo static/mismatch
    option resource = res-spoofed
    RW refs/heads/work/ = @engineer

repo dynamic/app
    option resource = res-dynamic-1
    R = @all
    RW+ refs/heads/work/ = @engineer

repo unmapped/no-option
    R = @all
`)}
	verifier := newFakeVerifier(log)
	service, err := git.NewService(git.ServiceConfig{
		TenantID:  testTenant,
		Storage:   storage,
		Policies:  policies,
		Executor:  executor,
		Observer:  &fakeObserver{executor: executor, log: log},
		Journal:   newFakeJournal(log),
		Verify:    verifier,
		Retention: &fakeRetention{log: log},
		Clock:     &fakeClock{now: testNow},
		ResourceIDs: map[string]string{
			"static/only":     "res-static-only",
			"static/agreed":   "res-agreed",
			"static/mismatch": "res-pinned",
		},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()

	// (a) Static ResourceIDs[repo] without policy option.
	if got, err := service.ResolveResourceID(ctx, "static/only"); err != nil || got != "res-static-only" {
		t.Fatalf("ResolveResourceID(static/only) = %q, %v; want res-static-only", got, err)
	}
	// Static and policy agree.
	if got, err := service.ResolveResourceID(ctx, "static/agreed"); err != nil || got != "res-agreed" {
		t.Fatalf("ResolveResourceID(static/agreed) = %q, %v; want res-agreed", got, err)
	}
	// (b) Dynamic policy option resource without static pin.
	if got, err := service.ResolveResourceID(ctx, "dynamic/app"); err != nil || got != "res-dynamic-1" {
		t.Fatalf("ResolveResourceID(dynamic/app) = %q, %v; want res-dynamic-1", got, err)
	}

	// Static vs policy mismatch refuses with fault.Forbidden + ErrResourceMismatch.
	_, err = service.ResolveResourceID(ctx, "static/mismatch")
	if !errors.Is(err, git.ErrResourceMismatch) {
		t.Fatalf("ResolveResourceID(static/mismatch) = %v; want ErrResourceMismatch", err)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("mismatch class = %v (ok=%v); want forbidden", class, ok)
	}

	// Update / Prepare also refuses static-vs-policy mismatch and request-vs-policy mismatch.
	verifier.script(testCredential, humanScript("prn-eng", "engineer"))
	_, err = service.Update(ctx, git.UpdateRequest{
		ResourceID:      "res-pinned",
		Repository:      "static/mismatch",
		Ref:             "refs/heads/work/a",
		Candidate:       "cand-1",
		ActorCredential: testCredential,
		SourceEventID:   "evt-mismatch-1",
	})
	if !errors.Is(err, git.ErrResourceMismatch) {
		t.Fatalf("Update(static/mismatch) = %v; want ErrResourceMismatch", err)
	}
	_, err = service.Update(ctx, git.UpdateRequest{
		ResourceID:      "res-wrong",
		Repository:      "dynamic/app",
		Ref:             "refs/heads/work/a",
		Candidate:       "cand-1",
		ActorCredential: testCredential,
		SourceEventID:   "evt-mismatch-2",
	})
	if !errors.Is(err, git.ErrResourceMismatch) {
		t.Fatalf("Update(dynamic/app with wrong resource) = %v; want ErrResourceMismatch", err)
	}

	// Unmapped repositories (absent from policy or lacking option resource)
	// return UnregisteredRepositoryError (fault.Unauthenticated).
	for _, repo := range []string{"unmapped/no-option", "unknown/repo"} {
		_, err := service.ResolveResourceID(ctx, repo)
		if !errors.Is(err, git.ErrRepositoryUnregistered) {
			t.Fatalf("ResolveResourceID(%q) = %v; want ErrRepositoryUnregistered", repo, err)
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("ResolveResourceID(%q) class = %v (ok=%v); want unauthenticated", repo, class, ok)
		}
	}

	// Updating the committed policy adds a brand-new repository immediately
	// without recreating Service.
	policies.set(mustConfig(t, `
repo admin
    RW = @admin

repo brand-new
    option resource = res-brand-new
    R = @all
`))
	if got, err := service.ResolveResourceID(ctx, "brand-new"); err != nil || got != "res-brand-new" {
		t.Fatalf("ResolveResourceID(brand-new) = %q, %v; want res-brand-new", got, err)
	}
}
