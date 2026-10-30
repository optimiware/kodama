package state

import (
	"fmt"
	"regexp"
	"strings"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
)

var (
	namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,126}[a-z0-9])?$`)
	prefixPattern    = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*/)?$`)
)

// ValidateNamespace checks the stable state namespace.
func ValidateNamespace(namespace string) error {
	if !namespacePattern.MatchString(namespace) {
		return fmt.Errorf("%w: namespace must be a lowercase DNS label", ErrInvalid)
	}
	return nil
}

// ValidatePrefix checks a normalized relative key prefix. A non-empty prefix
// ends in "/" and contains no empty, "." or ".." segments.
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !prefixPattern.MatchString(prefix) {
		return fmt.Errorf("%w: prefix must be a normalized relative path ending in /", ErrInvalid)
	}
	for _, part := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: prefix must be a normalized relative path", ErrInvalid)
		}
	}
	return nil
}

// AbsoluteKey resolves a workload-relative key under the binding prefix. The
// HTTP API accepts relative keys; callers cannot widen the binding.
func AbsoluteKey(binding Binding, key string) (string, error) {
	if err := ValidatePrefix(binding.Prefix); err != nil {
		return "", err
	}
	if err := statev1.ValidateKey(key); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	absolute := binding.Prefix + key
	if len(absolute) > statev1.MaxKeyBytes {
		return "", fmt.Errorf("%w: the resolved key exceeds the maximum length", ErrInvalid)
	}
	return absolute, nil
}

// RelativeKey removes the binding prefix from a stored key. It fails closed if
// the stored key is outside the binding.
func RelativeKey(binding Binding, key string) (string, error) {
	if !strings.HasPrefix(key, binding.Prefix) {
		return "", fmt.Errorf("%w: stored key is outside the binding", ErrDenied)
	}
	return strings.TrimPrefix(key, binding.Prefix), nil
}

// AbsolutePrefix resolves a workload-relative list prefix under the binding.
func AbsolutePrefix(binding Binding, prefix string) (string, error) {
	if err := ValidatePrefix(binding.Prefix); err != nil {
		return "", err
	}
	if err := ValidatePrefix(prefix); err != nil {
		return "", err
	}
	return binding.Prefix + prefix, nil
}
