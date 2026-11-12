package git_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	git "github.com/optimiware/kodama/internal/userland/git"
)

func pathSegments(path string) []string {
	return strings.Split(filepath.Clean(path), string(filepath.Separator))
}

func containsSegment(path, segment string) bool {
	return slices.Contains(pathSegments(path), segment)
}

// TestLocalStorageTenantIsolation pins per-tenant storage: the same repository
// name under two tenants resolves to two directories, each strictly inside the
// tenant's own area of the storage root.
func TestLocalStorageTenantIsolation(t *testing.T) {
	root := t.TempDir()
	storage, err := git.NewLocalStorage(root)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	tenantA := id.TenantID("tenant-a")
	tenantB := id.TenantID("tenant-b")

	pathA, err := storage.RepositoryPath(tenantA, "shop/web")
	if err != nil {
		t.Fatalf("RepositoryPath(tenant-a): %v", err)
	}
	pathB, err := storage.RepositoryPath(tenantB, "shop/web")
	if err != nil {
		t.Fatalf("RepositoryPath(tenant-b): %v", err)
	}
	if pathA == pathB {
		t.Fatalf("tenants share the repository path %q", pathA)
	}
	for name, path := range map[string]string{"tenant-a": pathA, "tenant-b": pathB} {
		if !filepath.IsAbs(path) {
			t.Fatalf("%s path %q is not absolute", name, path)
		}
		if !strings.HasPrefix(path, filepath.Clean(root)+string(filepath.Separator)) {
			t.Fatalf("%s path %q is outside the storage root %q", name, path, root)
		}
	}
	if !containsSegment(pathA, string(tenantA)) {
		t.Fatalf("tenant-a path %q does not carry the tenant segment", pathA)
	}
	if !containsSegment(pathB, string(tenantB)) {
		t.Fatalf("tenant-b path %q does not carry the tenant segment", pathB)
	}
	if containsSegment(pathB, string(tenantA)) || containsSegment(pathA, string(tenantB)) {
		t.Fatalf("tenant paths cross: %q / %q", pathA, pathB)
	}

	locationA, err := storage.EnsureRepository(tenantA, "shop/web")
	if err != nil {
		t.Fatalf("EnsureRepository(tenant-a): %v", err)
	}
	if !locationA.Created || locationA.Path != pathA {
		t.Fatalf("EnsureRepository(tenant-a) = %+v; want created at %q", locationA, pathA)
	}
	if info, err := os.Stat(pathA); err != nil || !info.IsDir() {
		t.Fatalf("tenant-a repository directory: %v", err)
	}
	again, err := storage.EnsureRepository(tenantA, "shop/web")
	if err != nil {
		t.Fatalf("second EnsureRepository: %v", err)
	}
	if again.Created || again.Path != pathA {
		t.Fatalf("second EnsureRepository = %+v; want existing at %q", again, pathA)
	}

	locationB, err := storage.EnsureRepository(tenantB, "shop/web")
	if err != nil {
		t.Fatalf("EnsureRepository(tenant-b): %v", err)
	}
	if !locationB.Created || locationB.Path != pathB {
		t.Fatalf("EnsureRepository(tenant-b) = %+v; want created at %q", locationB, pathB)
	}
	if info, err := os.Stat(pathB); err != nil || !info.IsDir() {
		t.Fatalf("tenant-b repository directory: %v", err)
	}
	if strings.HasPrefix(pathA, pathB+string(filepath.Separator)) || strings.HasPrefix(pathB, pathA+string(filepath.Separator)) {
		t.Fatalf("one tenant directory contains the other: %q / %q", pathA, pathB)
	}
}

// TestLocalStorageEnumeratesTenantRepositories pins repository discovery from
// the storage layout: nested names are returned tenant-relative and sorted, a
// missing tenant is an empty no-op, another tenant is never visible, and a
// directory that is not a bare repository (or a hidden provisioning leftover)
// is skipped.
func TestLocalStorageEnumeratesTenantRepositories(t *testing.T) {
	root := t.TempDir()
	storage, err := git.NewLocalStorage(root)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	tenant := id.TenantID("tenant-a")
	seedBare := func(relative string) {
		path := filepath.Join(root, string(tenant), filepath.FromSlash(relative))
		for _, name := range []string{"objects", "refs"} {
			if err := os.MkdirAll(filepath.Join(path, name), 0o750); err != nil {
				t.Fatalf("mkdir %s: %v", name, err)
			}
		}
		if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
			t.Fatalf("write HEAD: %v", err)
		}
	}
	seedBare("shop/web")
	seedBare("admin")
	// Not a repository: a directory with no bare layout.
	if err := os.MkdirAll(filepath.Join(root, string(tenant), "shop", "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	// A hidden provisioning leftover must never be enumerated.
	seedBareHidden := filepath.Join(root, string(tenant), ".provision-repository-leftover")
	if err := os.MkdirAll(seedBareHidden, 0o750); err != nil {
		t.Fatal(err)
	}
	other := id.TenantID("tenant-b")
	seedOther := filepath.Join(root, string(other), "shop", "web")
	for _, name := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(seedOther, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(seedOther, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repositories, err := storage.Repositories(tenant)
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if got, want := strings.Join(repositories, ","), "admin,shop/web"; got != want {
		t.Fatalf("Repositories = %q; want %q", got, want)
	}

	missing, err := storage.Repositories(id.TenantID("tenant-missing"))
	if err != nil {
		t.Fatalf("Repositories(missing tenant) = %v; want a no-op", err)
	}
	if len(missing) != 0 {
		t.Fatalf("Repositories(missing tenant) = %v; want empty", missing)
	}
	if _, err := storage.Repositories("../tenant"); err == nil {
		t.Fatal("Repositories accepted a traversal tenant")
	}
}

// TestLocalStorageRefusesEscapes pins that repository and tenant names cannot
// escape the storage root or collapse into another tenant's namespace.
func TestLocalStorageRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	storage, err := git.NewLocalStorage(root)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	tenant := id.TenantID("tenant-a")

	repositories := []string{
		"", ".", "..", "../evil", "shop/../evil", "shop//web", "/srv/git", "shop/", "shop/web/",
		"Shop/Web", "shop web", "shop/*", "shop/web\x00", strings.Repeat("a", 200),
	}
	for _, repository := range repositories {
		if _, err := storage.RepositoryPath(tenant, repository); err == nil {
			t.Errorf("RepositoryPath accepted %q", repository)
		}
		if _, err := storage.EnsureRepository(tenant, repository); err == nil {
			t.Errorf("EnsureRepository accepted %q", repository)
		}
	}

	tenants := []id.TenantID{"", ".", "..", "../tenant", "tenant/other", "/tenant", "tenant a"}
	for _, candidate := range tenants {
		if _, err := storage.RepositoryPath(candidate, "shop/web"); err == nil {
			t.Errorf("RepositoryPath accepted tenant %q", candidate)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read storage root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused names created %d entries under the root", len(entries))
	}
	if _, err := git.NewLocalStorage(""); err == nil {
		t.Fatal("NewLocalStorage accepted an empty root")
	}
}

// TestProvisionIsTenantScoped pins the provisioning boundary: a repository is
// created only under the bound tenant, provisioning is idempotent, and a new
// repository may be provisioned before its rules exist.
func TestProvisionIsTenantScoped(t *testing.T) {
	f := newFixture(t)
	location, err := f.service.Provision(context.Background(), "shop/web")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !location.Created || location.Path != f.repositoryPath("shop/web") {
		t.Fatalf("Provision = %+v; want created at %q", location, f.repositoryPath("shop/web"))
	}
	if info, err := os.Stat(location.Path); err != nil || !info.IsDir() {
		t.Fatalf("provisioned repository: %v", err)
	}
	again, err := f.service.Provision(context.Background(), "shop/web")
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if again.Created {
		t.Fatalf("second Provision = %+v; want Created=false", again)
	}

	fresh, err := f.service.Provision(context.Background(), "shop/new")
	if err != nil {
		t.Fatalf("Provision of a repository without rules: %v", err)
	}
	if !fresh.Created || fresh.Path != f.repositoryPath("shop/new") {
		t.Fatalf("Provision(shop/new) = %+v", fresh)
	}

	if _, err := f.service.Provision(context.Background(), "../escape"); !errors.Is(err, git.ErrInvalidRepository) {
		t.Fatalf("Provision escape error = %v; want ErrInvalidRepository", err)
	}

	// A second tenant service with the same root and repository name provisions
	// its own directory, never the first tenant's.
	otherStorage, err := git.NewLocalStorage(f.root)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	other := &fixture{
		t:         t,
		tenant:    id.TenantID("tenant-other"),
		root:      f.root,
		storage:   otherStorage,
		policies:  &fakePolicySource{config: mustConfig(t, testGitConfig)},
		executor:  newFakeExecutor(&callLog{}),
		journal:   newFakeJournal(&callLog{}),
		verifier:  newFakeVerifier(&callLog{}),
		retention: &fakeRetention{log: &callLog{}},
		clock:     &fakeClock{now: testNow},
		log:       &callLog{},
	}
	other.observer = &fakeObserver{executor: other.executor, log: other.log}
	other.service = other.newService()
	otherLocation, err := other.service.Provision(context.Background(), "shop/web")
	if err != nil {
		t.Fatalf("other tenant Provision: %v", err)
	}
	if otherLocation.Path == location.Path {
		t.Fatalf("two tenants provisioned the same path %q", location.Path)
	}
	if !containsSegment(otherLocation.Path, string(other.tenant)) {
		t.Fatalf("other tenant path %q does not carry its tenant segment", otherLocation.Path)
	}
}
