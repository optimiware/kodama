//go:build component

// OpenTofu validation for every deployment root under deploy/{aws,gcp,scw}.
// These are component tests because they start `tofu` and, for
// `init -backend=false`, download providers from the public registry; `make
// check` runs no process-starting tests, so the package carries the component
// build tag.
//
// The roots are discovered by walking the tree for a .tf file with a
// `terraform {` or `provider "x" {` block. Discovery alone is not enough: a new
// root must not silently go unvalidated, so the discovered set is also compared
// with expectedTofuRoots. Every discovered root then runs `tofu fmt -check
// -recursive`, `tofu init -backend=false -input=false` and `tofu validate`.
//
// Network: provider download during init is the one allowed network use.
// Nothing here reads or writes a backend or state file, and `plan`, `apply`,
// `destroy`, `import` and `refresh` never run. If the sandbox has no registry
// access, init fails and says so; a provider mirror can be pre-seeded through
// TF_CLI_CONFIG_FILE (inherited) or KODAMA_TOFU_PLUGIN_CACHE_DIR can be set to
// a shared cache.
package deploy

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// expectedTofuRoots is the explicit inventory of OpenTofu roots that must be
// validated. It is deliberately duplicated with the walk in
// discoverTofuRoots: a new root under deploy/{aws,gcp,scw} fails the discovery
// test until it is added here and therefore validated, and a deleted or
// renamed root fails too instead of being skipped.
var expectedTofuRoots = []string{
	"aws",
	"aws/bootstrap",
	"gcp",
	"gcp/bootstrap",
	"scw",
	"scw/bootstrap",
}

// tofuKnownFailures is the explicit, commented known-failures table. An entry
// lists the owner and the exact error text expected; the root then passes as a
// recorded finding instead of turning the suite red. A root that starts
// passing while listed here is an error (a stale table would hide the
// failure), and a root that fails without an entry is an error. It is empty:
// every discovered root validates.
var tofuKnownFailures = map[string]struct {
	owner string
	want  string
}{}

// assertExpectedTofuRoots compares the discovered roots with the explicit
// inventory and fails with the paths that need attention.
func assertExpectedTofuRoots(t *testing.T, discovered []string) {
	t.Helper()
	got := append([]string(nil), discovered...)
	want := append([]string(nil), expectedTofuRoots...)
	sort.Strings(got)
	sort.Strings(want)
	if slices.Equal(got, want) {
		return
	}
	var added, missing []string
	for _, root := range got {
		if !slices.Contains(want, root) {
			added = append(added, root)
		}
	}
	for _, root := range want {
		if !slices.Contains(got, root) {
			missing = append(missing, root)
		}
	}
	t.Fatalf("discovered OpenTofu roots do not match expectedTofuRoots:\n  discovered but unlisted (add to expectedTofuRoots): %s\n  listed but missing: %s", strings.Join(added, ", "), strings.Join(missing, ", "))
}

func TestTofuRootsAreDiscoveredAndListed(t *testing.T) {
	root := repoRoot(t)
	discovered := discoverTofuRoots(t, root+"/deploy")
	assertExpectedTofuRoots(t, discovered)
	for _, cloud := range []string{"aws", "gcp", "scw"} {
		for _, required := range []string{cloud, cloud + "/bootstrap"} {
			if !slices.Contains(discovered, required) {
				t.Errorf("%s must be discovered as an OpenTofu root", required)
			}
		}
	}
}

// TestTofuRootsAreFormatted checks every discovered root (and, through
// -recursive, its nested files) without initializing providers. The copied tree
// keeps the candidate clean.
func TestTofuRootsAreFormatted(t *testing.T) {
	tofuBinary(t)
	root := repoRoot(t)
	discovered := discoverTofuRoots(t, root+"/deploy")
	assertExpectedTofuRoots(t, discovered)
	copied := copyDeployTree(t, root)
	env := tofuProcessEnv(tofuPluginCacheDir(t))

	checked := make(map[string]bool, len(discovered))
	for _, relative := range discovered {
		t.Run(tofuRootSubtestName(relative), func(t *testing.T) {
			dir := filepath.Join(copied, filepath.FromSlash(relative))
			output, code := runTofu(t, dir, env, 2*time.Minute, "fmt", "-check", "-recursive")
			if code != 0 {
				t.Errorf("tofu fmt -check -recursive failed for deploy/%s (run `tofu fmt -recursive deploy/%s`):\n%s", relative, relative, output)
			}
			checked[relative] = true
		})
	}
	for _, relative := range discovered {
		if !checked[relative] {
			t.Errorf("OpenTofu root deploy/%s was never formatted-checked; a skipped root must not pass silently", relative)
		}
	}
}

// TestTofuRootsInitAndValidate runs the real initialization and validation.
// `-backend=false` is mandatory: these checks never read or write a state file.
// A failed root is reported with the exact root and the tofu output; a root in
// tofuKnownFailures passes only when the output carries the recorded error, so
// a green suite cannot hide a failing or silently skipped root.
func TestTofuRootsInitAndValidate(t *testing.T) {
	tofuBinary(t)
	root := repoRoot(t)
	discovered := discoverTofuRoots(t, root+"/deploy")
	assertExpectedTofuRoots(t, discovered)
	copied := copyDeployTree(t, root)
	env := tofuProcessEnv(tofuPluginCacheDir(t))

	validated := make(map[string]bool, len(discovered))
	for _, relative := range discovered {
		t.Run(tofuRootSubtestName(relative), func(t *testing.T) {
			dir := filepath.Join(copied, filepath.FromSlash(relative))
			known, knownRoot := tofuKnownFailures[relative]
			initOutput, initCode := runTofu(t, dir, env, 15*time.Minute, "init", "-backend=false", "-input=false")
			if initCode != 0 {
				// A registry failure is not a validation finding. Say how to
				// pre-seed a mirror so the two can be told apart.
				t.Fatalf("tofu init -backend=false failed for deploy/%s; if the sandbox has no registry access, set TF_CLI_CONFIG_FILE to a provider mirror or KODAMA_TOFU_PLUGIN_CACHE_DIR to a pre-populated cache:\n%s", relative, initOutput)
			}
			validateOutput, validateCode := runTofu(t, dir, env, 5*time.Minute, "validate")
			if validateCode == 0 {
				if knownRoot {
					t.Errorf("deploy/%s is listed in tofuKnownFailures but now validates; remove the stale %s finding", relative, known.owner)
				}
				validated[relative] = true
				return
			}
			if knownRoot && strings.Contains(validateOutput, known.want) {
				t.Logf("known validation failure for deploy/%s owned by %s (recorded, not fixed here):\n%s", relative, known.owner, validateOutput)
				validated[relative] = true
				return
			}
			t.Errorf("tofu validate failed for deploy/%s:\n%s", relative, validateOutput)
		})
	}
	for _, relative := range discovered {
		if !validated[relative] {
			t.Errorf("OpenTofu root deploy/%s was never initialized and validated; a skipped root must not pass silently", relative)
		}
	}
}
