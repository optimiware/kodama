package native

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
)

func TestSafeStorageRejectsSymlinkedCrossTenantRepository(t *testing.T) {
	root := t.TempDir()
	tenantRoot := filepath.Join(root, "tenant-a")
	if err := os.MkdirAll(tenantRoot, 0o750); err != nil {
		t.Fatalf("MkdirAll tenant: %v", err)
	}
	other := filepath.Join(root, "tenant-b", "secret")
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatalf("MkdirAll other: %v", err)
	}
	link := filepath.Join(tenantRoot, "linked")
	if err := os.Symlink(other, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	local, err := git.NewLocalStorage(root)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	storage, err := NewSafeStorage(local, root, "tenant-a")
	if err != nil {
		t.Fatalf("NewSafeStorage: %v", err)
	}
	if _, err := storage.RepositoryPath("tenant-a", "linked"); !errors.Is(err, git.ErrInvalidRepository) {
		t.Fatalf("symlinked repository error = %v; want ErrInvalidRepository", err)
	}
	if _, err := storage.RepositoryPath("tenant-a", "normal"); err != nil {
		t.Fatalf("normal repository: %v", err)
	}
	location, err := storage.EnsureRepository("tenant-a", "normal")
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if _, err := os.Stat(location.Path); err != nil {
		t.Fatalf("EnsureRepository did not create %q: %v", location.Path, err)
	}
}

func TestSafeStorageRejectsTenantDirectorySymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// The whole tenant directory is a symlink outside the storage root. The
	// guarded view must resolve it and refuse.
	if err := os.Symlink(outside, filepath.Join(root, "tenant-a")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	local, err := git.NewLocalStorage(root)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	storage, err := NewSafeStorage(local, root, "tenant-a")
	if err != nil {
		t.Fatalf("NewSafeStorage: %v", err)
	}
	if _, err := storage.RepositoryPath("tenant-a", "repo"); !errors.Is(err, git.ErrInvalidRepository) {
		t.Fatalf("escaped tenant root error = %v; want ErrInvalidRepository", err)
	}
}
