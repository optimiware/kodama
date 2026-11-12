// Package archtest implements the repository's static architecture checks.
//
// The checker is intentionally small and uses only the standard library. It
// inspects source, configuration and Markdown; it never
// starts product infrastructure or contacts a network service.
package archtest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Finding is one root-relative static check failure.
type Finding struct {
	Path    string
	Message string
}

func (f Finding) String() string { return f.Path + ": " + f.Message }

var checkNames = []string{
	"imports",
	"docs",
}

// UsageError reports invalid checker invocation.
type UsageError struct{ Message string }

func (e *UsageError) Error() string { return e.Message }

// Run executes the requested checks against root.
func Run(root string, checks []string) ([]Finding, error) {
	if len(checks) == 0 {
		return nil, &UsageError{Message: "no checks requested; known checks: " + strings.Join(checkNames, ", ")}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, &UsageError{Message: fmt.Sprintf("root %q is not a directory", root)}
	}

	requested := make(map[string]bool, len(checks))
	for _, name := range checks {
		if !slices.Contains(checkNames, name) {
			return nil, &UsageError{Message: fmt.Sprintf("unknown check %q; known checks: %s", name, strings.Join(checkNames, ", "))}
		}
		requested[name] = true
	}

	var findings []Finding
	for _, name := range checkNames {
		if !requested[name] {
			continue
		}
		var result []Finding
		var err error
		switch name {
		case "imports":
			result, err = checkImports(abs)
		case "docs":
			result, err = checkDocs(abs)
		}
		if err != nil {
			return nil, err
		}
		findings = append(findings, result...)
	}
	sortFindings(findings)
	return findings, nil
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path == findings[j].Path {
			return findings[i].Message < findings[j].Message
		}
		return findings[i].Path < findings[j].Path
	})
}

func relativePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func oneLine(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}
