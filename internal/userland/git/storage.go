package git

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/id"
)

// Storage resolves and provisions tenant-scoped repository directories. The
// tenant is one path segment, so two tenants never share a directory for the
// same repository name.
type Storage interface {
	RepositoryPath(id.TenantID, string) (string, error)
	EnsureRepository(id.TenantID, string) (RepositoryLocation, error)
}

// RepositoryLocation is one provisioned repository directory.
type RepositoryLocation struct {
	Path    string
	Created bool
}

// LocalStorage is the filesystem-backed tenant storage. It performs no Git
// command; it only validates names and manages the tenant directories.
type LocalStorage struct {
	root string
}

// NewLocalStorage creates the storage root and returns tenant-scoped storage.
func NewLocalStorage(root string) (*LocalStorage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("git storage root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve git storage root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("create git storage root: %w", err)
	}
	return &LocalStorage{root: absolute}, nil
}

// RepositoryPath validates the tenant and tenant-relative repository name and
// returns an absolute path strictly inside the storage root. Traversal,
// absolute names, empty segments, uppercase, spaces, glob characters and
// parent segments refuse before any filesystem access.
func (storage *LocalStorage) RepositoryPath(tenant id.TenantID, repository string) (string, error) {
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return "", fmt.Errorf("%w: tenant %q", ErrInvalidRepository, tenant)
	}
	if err := validateRepositoryName(repository); err != nil {
		return "", err
	}
	path := filepath.Join(storage.root, string(tenant), filepath.FromSlash(repository))
	if !withinRoot(storage.root, path) {
		return "", fmt.Errorf("%w: repository %q escapes the storage root", ErrInvalidRepository, repository)
	}
	return path, nil
}

// Repositories lists every bare repository under one tenant's directory as a
// tenant-relative name, in lexical order. A tenant with no directory yet has no
// repository: that is reported as an empty list, never an error, so a periodic
// sweep treats it as a no-op. Hidden provisioning leftovers and anything that
// is not a bare repository are skipped.
func (storage *LocalStorage) Repositories(tenant id.TenantID) ([]string, error) {
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidRepository, tenant)
	}
	base := filepath.Join(storage.root, string(tenant))
	info, err := os.Lstat(base)
	switch {
	case os.IsNotExist(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("inspect tenant directory: %w", err)
	case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: tenant %q is not a directory", ErrInvalidRepository, tenant)
	}
	repositories := make([]string, 0)
	err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || path == base {
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return filepath.SkipDir
		}
		if !bareRepositoryDirectory(path) {
			return nil
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		repository := filepath.ToSlash(relative)
		if err := validateRepositoryName(repository); err != nil {
			// A directory outside the repository grammar is not served by this
			// storage boundary. Skip it instead of failing the whole tenant.
			return filepath.SkipDir
		}
		repositories = append(repositories, repository)
		// A bare repository contains only private Git state; never descend
		// into refs/, objects/ or hooks/ looking for nested repositories.
		return filepath.SkipDir
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate tenant repositories: %w", err)
	}
	sort.Strings(repositories)
	return repositories, nil
}

// bareRepositoryDirectory reports whether path has the direct layout of a bare
// Git repository. Symlinks are not followed: only a real directory with HEAD,
// objects and refs qualifies.
func bareRepositoryDirectory(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	for _, name := range []string{"HEAD", "objects", "refs"} {
		child, err := os.Lstat(filepath.Join(path, name))
		if err != nil {
			return false
		}
		if name == "HEAD" {
			if !child.Mode().IsRegular() {
				return false
			}
			continue
		}
		if !child.IsDir() {
			return false
		}
	}
	return true
}

// EnsureRepository creates the tenant-scoped repository directory and parents.
// A second call reports Created=false. It never touches another tenant's area.
func (storage *LocalStorage) EnsureRepository(tenant id.TenantID, repository string) (RepositoryLocation, error) {
	path, err := storage.RepositoryPath(tenant, repository)
	if err != nil {
		return RepositoryLocation{}, err
	}
	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		if !info.IsDir() {
			return RepositoryLocation{}, fmt.Errorf("repository path %q is not a directory", path)
		}
		return RepositoryLocation{Path: path, Created: false}, nil
	case !os.IsNotExist(statErr):
		return RepositoryLocation{}, fmt.Errorf("stat repository path %q: %w", path, statErr)
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return RepositoryLocation{}, fmt.Errorf("create repository path %q: %w", path, err)
	}
	return RepositoryLocation{Path: path, Created: true}, nil
}

// validateRepositoryName checks a tenant-relative repository name. It refuses
// the bare dot segment, absolute or trailing paths, empty segments, uppercase,
// spaces, glob characters and any parent segment.
func validateRepositoryName(value string) error {
	if value == "." || !validRepositoryName(value) {
		return fmt.Errorf("%w: repository %q", ErrInvalidRepository, value)
	}
	return nil
}

// withinRoot reports whether path stays strictly inside root.
func withinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if relative == "." || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
