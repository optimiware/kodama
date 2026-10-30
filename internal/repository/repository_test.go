package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	t0    = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	me    = Actor{Type: ActorUser, ID: "usr_1"}
)

func reserved(t *testing.T) *Repository {
	t.Helper()
	r, err := New(NewParams{ID: "repo_1", Scope: scope, Name: "backend", CreatedBy: me}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ready(t *testing.T) *Repository {
	t.Helper()
	r := reserved(t)
	if err := r.CompleteProvisioning(true, t0, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewReservesAProvisioningRepositoryWithTheDefaultsDeny(t *testing.T) {
	r := reserved(t)
	if r.Status != gitv1.StatusProvisioning || r.Reason != ReasonProvisioningPending || r.Version != 1 || r.Empty ||
		r.DefaultBranch != "main" || r.Hosting != gitv1.HostingInternal || r.Policy.DirectPushToDefault {
		t.Fatalf("%+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []NewParams{
		{ID: "repo_1", Scope: scope, Name: "Bad", CreatedBy: me},
		{ID: "repo_1", Scope: scope, Name: "ok", DefaultBranch: "kodama/x", CreatedBy: me},
		{ID: "bad id", Scope: scope, Name: "ok", CreatedBy: me},
		{ID: "repo_1", Scope: domain.Scope{TenantID: "t"}, Name: "ok", CreatedBy: me},
		{ID: "repo_1", Scope: scope, Name: "ok"},
	} {
		if _, err := New(p, t0); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

func TestTransitionsAreExplicit(t *testing.T) {
	r := reserved(t)
	if _, err := r.Archive(t0); !errors.Is(err, ErrNotReady) {
		t.Fatalf("archive while provisioning: %v", err)
	}
	if _, err := r.Restore(t0); !errors.Is(err, ErrNotReady) {
		t.Fatalf("restore while provisioning: %v", err)
	}
	if _, err := r.ChangeDefaultBranch("trunk", t0); !errors.Is(err, ErrNotReady) {
		t.Fatalf("branch while provisioning: %v", err)
	}
	if _, err := r.SetPolicy(gitv1.RepositoryPolicy{DirectPushToDefault: true}, t0); !errors.Is(err, ErrNotReady) {
		t.Fatalf("policy while provisioning: %v", err)
	}
	if _, err := r.ObserveEmpty(false, t0, t0); !errors.Is(err, ErrNotReady) {
		t.Fatalf("report while provisioning: %v", err)
	}
	if changed, err := r.RecordProvisioningReason(ReasonWorkerUnavailable, t0); err != nil || !changed || r.Version != 2 {
		t.Fatalf("reason: %v %v %d", changed, err, r.Version)
	}
	if changed, err := r.RecordProvisioningReason(ReasonWorkerUnavailable, t0); err != nil || changed || r.Version != 2 {
		t.Fatalf("the same reason again: %v %v %d", changed, err, r.Version)
	}
	if err := r.CompleteProvisioning(true, t0, t0); err != nil || r.Status != gitv1.StatusReady || r.Reason != "" || r.Version != 3 || !r.Empty {
		t.Fatalf("complete: %v %+v", err, r)
	}
	if err := r.CompleteProvisioning(true, t0, t0); err == nil {
		t.Fatal("a ready repository completed provisioning again")
	}
	if _, err := r.RecordProvisioningReason(ReasonWorkerUnavailable, t0); err == nil {
		t.Fatal("a reason was recorded on a ready repository")
	}

	if changed, err := r.Archive(t0); err != nil || !changed || r.Status != gitv1.StatusArchived || r.Reason != ReasonArchived {
		t.Fatalf("archive: %v %v %+v", changed, err, r)
	}
	if changed, err := r.Archive(t0); err != nil || changed {
		t.Fatalf("archive twice: %v %v", changed, err)
	}
	if _, err := r.ChangeDefaultBranch("trunk", t0); !errors.Is(err, ErrArchived) {
		t.Fatalf("branch while archived: %v", err)
	}
	if changed, err := r.Restore(t0); err != nil || !changed || r.Status != gitv1.StatusReady || r.Reason != "" {
		t.Fatalf("restore: %v %v %+v", changed, err, r)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	// The status graph is the contract's: no path back to provisioning.
	for _, to := range []gitv1.RepositoryStatus{gitv1.StatusProvisioning} {
		for _, from := range []gitv1.RepositoryStatus{gitv1.StatusReady, gitv1.StatusArchived} {
			if gitv1.ValidRepositoryTransition(from, to) {
				t.Errorf("%s to %s is allowed", from, to)
			}
		}
	}
}

func TestPolicyAndBranchChangesBumpOnlyWhenTheyChangeSomething(t *testing.T) {
	r := ready(t)
	v := r.Version
	if changed, err := r.SetPolicy(gitv1.RepositoryPolicy{}, t0); err != nil || changed || r.Version != v {
		t.Fatalf("the default policy again: %v %v", changed, err)
	}
	if changed, err := r.SetPolicy(gitv1.RepositoryPolicy{DirectPushToDefault: true}, t0); err != nil || !changed || r.Version != v+1 {
		t.Fatalf("enable: %v %v", changed, err)
	}
	if changed, err := r.ChangeDefaultBranch("main", t0); err != nil || changed {
		t.Fatalf("the same branch: %v %v", changed, err)
	}
	if _, err := r.ChangeDefaultBranch("refs/heads/main", t0); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("a ref path: %v", err)
	}
}

func TestEmptinessReportsNeverMoveBack(t *testing.T) {
	r := ready(t) // observed at t0, empty
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	if changed, err := r.ObserveEmpty(false, at(2), t0); err != nil || !changed || r.Empty {
		t.Fatalf("refs: %v %v", changed, err)
	}
	v := r.Version
	if changed, err := r.ObserveEmpty(true, at(1), t0); err != nil || changed || r.Empty || r.Version != v {
		t.Fatalf("a stale report: %v %v", changed, err)
	}
	if changed, err := r.ObserveEmpty(false, at(4), t0); err != nil || changed || r.Version != v || !r.EmptyObservedAt.Equal(at(4)) {
		t.Fatalf("a newer confirmation: %v %v %v", changed, err, r.EmptyObservedAt)
	}
	if changed, err := r.ObserveEmpty(true, at(3), t0); err != nil || changed || r.Empty {
		t.Fatalf("a report older than the last observation: %v %v", changed, err)
	}
	if changed, err := r.ObserveEmpty(true, at(5), t0); err != nil || !changed || !r.Empty {
		t.Fatalf("a newer report: %v %v", changed, err)
	}
}

func TestExternalRecordsAreReadOnly(t *testing.T) {
	r, err := NewExternal(NewExternalParams{ID: "repo_ext", Scope: scope, Name: "legacy", URL: "https://git.example.test/acme/legacy.git", CreatedBy: me}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != gitv1.StatusReady || r.Reason != "" || r.Empty || r.Hosting != gitv1.HostingExternal || r.Policy.DirectPushToDefault {
		t.Fatalf("%+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, do := range map[string]func() error{
		"archive":  func() error { _, err := r.Archive(t0); return err },
		"restore":  func() error { _, err := r.Restore(t0); return err },
		"branch":   func() error { _, err := r.ChangeDefaultBranch("x", t0); return err },
		"policy":   func() error { _, err := r.SetPolicy(gitv1.RepositoryPolicy{DirectPushToDefault: true}, t0); return err },
		"empty":    func() error { _, err := r.ObserveEmpty(true, t0, t0); return err },
		"complete": func() error { return r.CompleteProvisioning(true, t0, t0) },
		"reason":   func() error { _, err := r.RecordProvisioningReason(ReasonWorkerUnavailable, t0); return err },
	} {
		if err := do(); !errors.Is(err, ErrHostingMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if r.Version != 1 {
		t.Fatalf("a refused change moved the version to %d", r.Version)
	}
	for _, url := range []string{"git@x.test:a/b.git", "ssh://x.test/a/b", "http://x.test/a", "https://u:p@x.test/a", "https://x.test/a b", ""} {
		if _, err := NewExternal(NewExternalParams{ID: "repo_ext", Scope: scope, Name: "legacy", URL: url, CreatedBy: me}, t0); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("%q: %v", url, err)
		}
	}
}

func TestValidateRefusesBrokenInvariants(t *testing.T) {
	for name, mutate := range map[string]func(*Repository){
		"a ready repository with a reason":        func(r *Repository) { r.Reason = "x" },
		"an archived repository without a reason": func(r *Repository) { r.Status, r.Reason = gitv1.StatusArchived, "" },
		"a provisioning repository that is empty": func(r *Repository) {
			r.Status, r.Reason, r.Empty = gitv1.StatusProvisioning, ReasonProvisioningPending, true
		},
		"an internal repository with a URL":    func(r *Repository) { r.ExternalURL = "https://x.test/a" },
		"an external repository without a URL": func(r *Repository) { r.Hosting = gitv1.HostingExternal },
		"an unknown status":                    func(r *Repository) { r.Status = "deleted" },
		"a zero version":                       func(r *Repository) { r.Version = 0 },
	} {
		r := ready(t)
		mutate(r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestEquivalenceIsWhatACreateAsksFor(t *testing.T) {
	a, b := reserved(t), reserved(t)
	if !a.Equivalent(b) {
		t.Fatal("the same request is not equivalent")
	}
	b.DefaultBranch = "trunk"
	if a.Equivalent(b) {
		t.Fatal("another branch is equivalent")
	}
	b = reserved(t)
	b.Policy.DirectPushToDefault = true
	if a.Equivalent(b) {
		t.Fatal("another policy is equivalent")
	}
	ext, err := NewExternal(NewExternalParams{ID: "repo_e", Scope: scope, Name: "backend", URL: "https://x.test/a", CreatedBy: me}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Equivalent(ext) || ext.Equivalent(a) {
		t.Fatal("an external record is equivalent to an internal request")
	}
}

func TestExternalNamesAreValidAndStable(t *testing.T) {
	name := regexp.MustCompile(gitv1.Patterns["RepositoryName"])
	for url, want := range map[string]string{
		"https://git.example.test/acme/Backend.git":           "backend",
		"https://git.example.test/acme/backend/":              "backend",
		"https://git.example.test/acme/my_repo.v2.git":        "my-repo-v2",
		"https://git.example.test/acme/9lives":                "r-9lives",
		"https://git.example.test/acme/x.git?ref=main":        "x",
		"https://git.example.test":                            "git-example-test",
		"https://git.example.test/" + strings.Repeat("a", 90): strings.Repeat("a", 54),
	} {
		names := externalNames(ExternalConfig{ID: "cfg", URL: url})
		if names[0] != want {
			t.Errorf("%s: %q, want %q", url, names[0], want)
		}
		for _, n := range names {
			if !name.MatchString(n) {
				t.Errorf("%s: %q is not a repository name", url, n)
			}
		}
		if again := externalNames(ExternalConfig{ID: "cfg", URL: url}); again[1] != names[1] {
			t.Errorf("%s: the fallback name is not stable", url)
		}
	}
	if a, b := ExternalID(scope, "cfg"), ExternalID(domain.Scope{TenantID: scope.TenantID, ProjectID: "p_other"}, "cfg"); a == b || a.Validate() != nil || a != ExternalID(scope, "cfg") {
		t.Fatalf("external identifiers: %s %s", a, b)
	}
}

func TestEveryErrorMapsToAContractCode(t *testing.T) {
	for err, want := range map[error]domain.ErrorCode{
		ErrNotFound: domain.CodeNotFound, ErrDenied: domain.CodeForbidden, ErrVersionConflict: domain.CodeVersionConflict,
		ErrNameTaken: gitv1.CodeRepositoryNameTaken, ErrNotReady: gitv1.CodeRepositoryNotReady, ErrArchived: gitv1.CodeRepositoryArchived,
		ErrHostingMismatch: gitv1.CodeHostingMismatch, ErrUnavailable: domain.CodeUnavailable, ErrInvalidValue: domain.CodeInvalidRequest,
	} {
		got, ok := ContractCode(errors.Join(errors.New("context"), err))
		if !ok || got != want {
			t.Errorf("%v: %q %v", err, got, ok)
		}
		if _, known := gitv1.Status(got); !known {
			t.Errorf("%q has no HTTP status in the contract", got)
		}
	}
	if _, ok := ContractCode(errors.New("boom")); ok {
		t.Error("an unclassified error got a code")
	}
	if _, ok := ContractCode(nil); ok {
		t.Error("nil got a code")
	}
}

type nopStore struct{ Store }
type nopAuthz struct{}

func (nopAuthz) Authorize(context.Context, Actor, Action, domain.Scope, Resource) error { return nil }

type nopURLs struct{}

func (nopURLs) CloneURL(context.Context, domain.Scope, string) (string, error) { return "", nil }

func TestServiceFailsClosedWithoutItsPorts(t *testing.T) {
	full := Dependencies{Store: nopStore{}, Authorizer: nopAuthz{}, Hosts: nopHosts{}, CloneURLs: nopURLs{}}
	for name, mutate := range map[string]func(*Dependencies){
		"store": func(d *Dependencies) { d.Store = nil }, "authorizer": func(d *Dependencies) { d.Authorizer = nil },
		"hosts": func(d *Dependencies) { d.Hosts = nil }, "clone URLs": func(d *Dependencies) { d.CloneURLs = nil },
	} {
		d := full
		mutate(&d)
		if _, err := NewService(d, DefaultOptions()); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("without %s: %v", name, err)
		}
	}
	for _, o := range []Options{{}, {HostsDeadline: time.Second}, {ReconcileBatch: 1}, {HostsDeadline: time.Second, ReconcileBatch: 5000}} {
		if _, err := NewService(full, o); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	svc, err := NewService(full, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReconcileAll(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("a sweep without tenants: %v", err)
	}
	if _, _, err := svc.MapConfigured(context.Background(), me, scope); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("a mapping without a source: %v", err)
	}
}

type nopHosts struct{ Hosts }

func TestApplyPatchIsOneTransition(t *testing.T) {
	r := ready(t)
	v := r.Version
	trunk, on := "trunk", gitv1.RepositoryPolicy{DirectPushToDefault: true}
	before := r.Clone()
	res, err := r.ApplyPatch(Patch{DefaultBranch: &trunk, Policy: &on}, t0.Add(time.Minute))
	if err != nil || !res.BranchChanged || !res.PolicyChanged || res.BranchFrom != "main" || r.Version != v+1 || r.DefaultBranch != "trunk" || !r.Policy.DirectPushToDefault {
		t.Fatalf("%+v %v %+v", res, err, r)
	}
	if err := CheckChange(before, r); err != nil {
		t.Fatalf("a one-step patch fails the guard: %v", err)
	}
	// An error applies nothing.
	bad := "kodama/x"
	off := gitv1.RepositoryPolicy{}
	snapshot := r.Clone()
	if _, err := r.ApplyPatch(Patch{DefaultBranch: &bad, Policy: &off}, t0); !errors.Is(err, ErrInvalidValue) || *r != *snapshot {
		t.Fatalf("a failing patch changed the repository: %v", err)
	}
	// Nothing to change keeps the version.
	if res, err := r.ApplyPatch(Patch{DefaultBranch: &trunk, Policy: &on}, t0); err != nil || res.Changed() || *r != *snapshot {
		t.Fatalf("an unchanged patch: %+v %v", res, err)
	}
}

func TestCheckChangeIsTheStorageGuard(t *testing.T) {
	base := ready(t)
	step := func(mutate func(*Repository)) error {
		after := base.Clone()
		mutate(after)
		return CheckChange(base, after)
	}
	for name, mutate := range map[string]func(*Repository){
		"a change without a version step": func(r *Repository) { r.DefaultBranch = "x" },
		"a two-step version":              func(r *Repository) { r.DefaultBranch, r.Version = "x", r.Version+2 },
		"a rename":                        func(r *Repository) { r.Name, r.Version = "other", r.Version+1 },
		"a back-move to provisioning":     func(r *Repository) { r.Status, r.Version = gitv1.StatusProvisioning, r.Version+1 },
		"an observation moving back":      func(r *Repository) { r.EmptyObservedAt = base.EmptyObservedAt.Add(-time.Second) },
	} {
		if err := step(mutate); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, mutate := range map[string]func(*Repository){
		"one step":                 func(r *Repository) { r.DefaultBranch, r.Version = "x", r.Version+1 },
		"a newer observation only": func(r *Repository) { r.EmptyObservedAt = base.EmptyObservedAt.Add(time.Second) },
		"archive":                  func(r *Repository) { r.Status, r.Reason, r.Version = gitv1.StatusArchived, ReasonArchived, r.Version+1 },
	} {
		if err := step(mutate); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
