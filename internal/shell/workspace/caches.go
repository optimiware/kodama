package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/kernel/engine"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

// CacheRoot is the mount root for optional named non-authoritative dependency
// caches (/kodema/cache/<name>). It is strictly separate from disposable
// /workspace and read-only /kodema/* activation files.
const CacheRoot = "/kodema/cache"

// CachePath returns the confined in-container mount path (/kodema/cache/<name>)
// for one validated cache slug.
func CachePath(name string) (string, error) {
	if err := definition.ValidateCacheName(name); err != nil {
		return "", err
	}
	return CacheRoot + "/" + name, nil
}

// CacheScopeKey returns the deterministic, DNS-safe, collision-resistant cache
// storage identifier scoped strictly by (tenant_id, project_id, cache_name).
// Caches are never shared across tenants or projects.
func CacheScopeKey(scope tenancy.Scope, name string) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if !scope.ProjectScoped() {
		return "", fault.New(fault.InvalidInput, "cache scope must name both tenant and project")
	}
	if err := definition.ValidateCacheName(name); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("kodema:cache:v1\x00" + string(scope.TenantID) + "\x00" + string(scope.ProjectID) + "\x00" + name))
	return hex.EncodeToString(sum[:16]), nil
}

// BuildCacheMounts validates declared cache names and resolves their
// tenant/project-scoped CacheMount list.
func BuildCacheMounts(scope tenancy.Scope, names []string) ([]engine.CacheMount, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if err := definition.ValidateCaches(names); err != nil {
		return nil, err
	}
	mounts := make([]engine.CacheMount, len(names))
	for index, name := range names {
		key, err := CacheScopeKey(scope, name)
		if err != nil {
			return nil, err
		}
		mounts[index] = engine.CacheMount{Name: name, Key: key}
	}
	return mounts, nil
}

// ValidateCacheMounts validates that scoped cache mounts match declared
// container caches and carry valid names and tenant/project-scoped keys.
func ValidateCacheMounts(declared []string, mounts []engine.CacheMount) error {
	if err := definition.ValidateCaches(declared); err != nil {
		return err
	}
	if len(declared) > 0 && len(mounts) == 0 {
		return fault.New(fault.InvalidInput, "declared container caches require tenant/project-scoped cache mounts")
	}
	if len(mounts) > definition.MaxContainerCaches {
		return fault.New(fault.InvalidInput, fmt.Sprintf("container declares %d caches; at most %d are allowed", len(mounts), definition.MaxContainerCaches))
	}
	seenNames := make(map[string]bool, len(mounts))
	seenKeys := make(map[string]bool, len(mounts))
	for _, mount := range mounts {
		if err := definition.ValidateCacheName(mount.Name); err != nil {
			return err
		}
		if !validCacheKey(mount.Key) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("cache %q has invalid scoped key", mount.Name))
		}
		if seenNames[mount.Name] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("duplicate cache mount %q", mount.Name))
		}
		if seenKeys[mount.Key] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("duplicate cache key for %q", mount.Name))
		}
		seenNames[mount.Name] = true
		seenKeys[mount.Key] = true
	}
	if len(declared) > 0 {
		if len(declared) != len(mounts) {
			return fault.New(fault.InvalidInput, "cache mounts do not match declared container caches")
		}
		for index, name := range declared {
			if mounts[index].Name != name {
				return fault.New(fault.InvalidInput, "cache mounts do not match declared container caches")
			}
		}
	}
	return nil
}

func validCacheKey(key string) bool {
	if len(key) < 16 || len(key) > 64 {
		return false
	}
	for index := 0; index < len(key); index++ {
		ch := key[index]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}
