package runtimesupport

import (
	"regexp"
	"strings"
	"testing"
)

var runSuffixPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

// TestRunScopeIsStableWithinOneProcess pins the retry invariant: every call
// inside one test binary maps the same literal to the same run-scoped identity,
// and only the process-wide suffix differs from the literal.
func TestRunScopeIsStableWithinOneProcess(t *testing.T) {
	const base = "workspace-podman-1"
	first := RunScoped(base)
	if first != RunScoped(base) {
		t.Fatalf("RunScoped(%q) is not stable within the process: %q then %q", base, first, RunScoped(base))
	}
	scope := RunScope()
	if first != scope.ID(base) {
		t.Fatalf("RunScoped(%q) = %q; want RunScope().ID = %q", base, first, scope.ID(base))
	}
	if first == base {
		t.Fatalf("RunScoped(%q) = the literal base; a concurrent run would collide", base)
	}
	if !strings.HasPrefix(first, base+"-") {
		t.Fatalf("RunScoped(%q) = %q; want the base followed by the run suffix", base, first)
	}
	if got := strings.TrimPrefix(first, base+"-"); !runSuffixPattern.MatchString(got) {
		t.Fatalf("run suffix %q is not eight lowercase hex characters", got)
	}
	if other := scope.ID("workload-c1"); !strings.HasSuffix(other, "-"+scope.Suffix()) {
		t.Fatalf("a second base %q does not share the process run suffix %q", other, scope.Suffix())
	}
}

// TestScopeWithSuffixReconstructs pins deterministic reconstruction: the same
// suffix reproduces the same identities, which is how a fixture retry or a
// reconstructed controller reuses one workload.
func TestScopeWithSuffixReconstructs(t *testing.T) {
	first := ScopeWithSuffix("deadbeef")
	second := ScopeWithSuffix("deadbeef")
	if first.ID("execution-a") != second.ID("execution-a") {
		t.Fatalf("reconstructed scopes disagree: %q vs %q", first.ID("execution-a"), second.ID("execution-a"))
	}
	if got := first.ID("workspace-a"); got != "workspace-a-deadbeef" {
		t.Fatalf("ScopeWithSuffix(ID) = %q; want workspace-a-deadbeef", got)
	}
	if first.Suffix() != "deadbeef" {
		t.Fatalf("ScopeWithSuffix().Suffix() = %q", first.Suffix())
	}
}

// TestNewScopeIsIndependent pins the concurrency invariant: two scopes never
// share a suffix, so two runs started concurrently cannot derive the same
// deterministic Podman name from the same literals.
func TestNewScopeIsIndependent(t *testing.T) {
	seen := map[string]bool{RunScope().Suffix(): true}
	for range 16 {
		scope := NewScope()
		if !runSuffixPattern.MatchString(scope.Suffix()) {
			t.Fatalf("NewScope suffix %q is not eight lowercase hex characters", scope.Suffix())
		}
		if seen[scope.Suffix()] {
			t.Fatalf("NewScope repeated suffix %q", scope.Suffix())
		}
		seen[scope.Suffix()] = true
		if got := scope.ID("workspace-podman-1"); got == "workspace-podman-1" {
			t.Fatalf("NewScope did not namespace the literal: %q", got)
		}
	}
}
