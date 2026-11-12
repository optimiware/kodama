package native

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

// SafeStorage is the composed Git app's tenant storage view. It delegates name
// validation and provisioning to git.LocalStorage, then proves the
// resolved path stays inside the tenant directory even when a component is a
// symbolic link. A tenant can therefore never reach another tenant's
// repository through a symlinked directory.
type SafeStorage struct {
	inner  git.Storage
	root   string
	tenant id.TenantID
}

var _ git.Storage = (*SafeStorage)(nil)

// NewSafeStorage resolves the storage root and returns the guarded view.
func NewSafeStorage(inner git.Storage, root string, tenant id.TenantID) (*SafeStorage, error) {
	if inner == nil {
		return nil, fmt.Errorf("safe storage inner boundary is missing")
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("safe storage root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root %q: %w", root, err)
	}
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("safe storage tenant: %w", err)
	}
	return &SafeStorage{inner: inner, root: filepath.Clean(absolute), tenant: tenant}, nil
}

// RepositoryPath returns the guarded tenant-scoped repository path.
func (storage *SafeStorage) RepositoryPath(tenant id.TenantID, repository string) (string, error) {
	path, err := storage.inner.RepositoryPath(tenant, repository)
	if err != nil {
		return "", err
	}
	if err := storage.withinTenant(tenant, path); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureRepository creates the tenant-scoped repository directory and proves
// the result stays inside the tenant directory.
func (storage *SafeStorage) EnsureRepository(tenant id.TenantID, repository string) (git.RepositoryLocation, error) {
	location, err := storage.inner.EnsureRepository(tenant, repository)
	if err != nil {
		return git.RepositoryLocation{}, err
	}
	if err := storage.withinTenant(tenant, location.Path); err != nil {
		return git.RepositoryLocation{}, err
	}
	return location, nil
}

// withinTenant rejects a path that resolves outside `<root>/<tenant>`.
func (storage *SafeStorage) withinTenant(tenant id.TenantID, path string) error {
	resolvedRoot, err := resolveExisting(storage.root)
	if err != nil {
		return fmt.Errorf("%w: resolve storage root: %v", git.ErrInvalidRepository, err)
	}
	base := filepath.Join(storage.root, string(tenant))
	resolvedBase, err := resolveExisting(base)
	if err != nil {
		return fmt.Errorf("%w: resolve tenant root: %v", git.ErrInvalidRepository, err)
	}
	if !within(resolvedRoot, resolvedBase) {
		return fmt.Errorf("%w: tenant %q resolves outside the storage root", git.ErrInvalidRepository, tenant)
	}
	resolvedPath, err := resolveExisting(path)
	if err != nil {
		return fmt.Errorf("%w: resolve repository path: %v", git.ErrInvalidRepository, err)
	}
	if !within(resolvedBase, resolvedPath) {
		return fmt.Errorf("%w: repository %q resolves outside its tenant", git.ErrInvalidRepository, path)
	}
	return nil
}

// within reports whether path stays strictly inside base.
func within(base, path string) bool {
	relative, err := filepath.Rel(base, path)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// resolveExisting resolves symbolic links for the longest existing ancestor of
// path and appends the remaining segments. It never requires the full path to
// exist, so a not-yet-created repository is still checked.
func resolveExisting(path string) (string, error) {
	cleaned := filepath.Clean(path)
	var suffix []string
	current := cleaned
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
