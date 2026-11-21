package definition

import (
	"fmt"
	"strings"
)

const (
	// MaxContainerCaches bounds the number of named caches one container action
	// may declare.
	MaxContainerCaches = 8
	// MaxCacheNameLength bounds one declared cache name in bytes.
	MaxCacheNameLength = 63
)

// ValidateCacheName checks that name is a safe lowercase cache slug matching
// ^[a-z0-9][a-z0-9._-]{0,62}$ without path separators or ".." sequences.
func ValidateCacheName(name string) error {
	if name == "" {
		return invalidInput("cache name is empty")
	}
	if len(name) > MaxCacheNameLength {
		return invalidInput("cache name %q exceeds %d bytes", name, MaxCacheNameLength)
	}
	if strings.Contains(name, "..") {
		return invalidInput("cache name %q must not contain '..'", name)
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
		return invalidInput("cache name %q must start with a lowercase letter or digit", name)
	}
	for index := 1; index < len(name); index++ {
		ch := name[index]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '.', ch == '_', ch == '-':
		default:
			return invalidInput("cache name %q contains invalid character %q (must match ^[a-z0-9][a-z0-9._-]{0,62}$)", name, ch)
		}
	}
	return nil
}

// ValidateCaches validates the optional declared cache list of one container
// action: bounded count, valid lowercase slugs, no duplicates.
func ValidateCaches(caches []string) error {
	if len(caches) > MaxContainerCaches {
		return invalidInput("container declares %d caches; at most %d are allowed", len(caches), MaxContainerCaches)
	}
	seen := make(map[string]bool, len(caches))
	for _, name := range caches {
		if err := ValidateCacheName(name); err != nil {
			return err
		}
		if seen[name] {
			return invalidInput("duplicate cache name %q", name)
		}
		seen[name] = true
	}
	return nil
}

func validateContainerCaches(action string, caches []string) error {
	if len(caches) == 0 {
		return nil
	}
	if err := ValidateCaches(caches); err != nil {
		return invalidInput("action %q: %s", action, fmt.Sprint(err))
	}
	return nil
}
