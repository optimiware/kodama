package runtimesupport

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// runSuffixBytes is the number of random bytes in a run suffix. Eight hex
// characters keep the suffix readable in container/volume names while making a
// collision between two concurrent suite runs negligible.
const runSuffixBytes = 4

// Scope is one suite run's identity namespace. Every literal fixture identity
// is mapped through a scope exactly once, so the Podman adapter's deterministic
// kodama-workload-<digest> and kodama-workspace-<digest> names are unique per
// run instead of colliding between two concurrent runs that use the same
// literals. A scope is a value: reconstructing it from the same suffix yields
// the same identities, which is what a retry inside one fixture needs.
type Scope struct {
	suffix string
}

// RunScope is this test process's scope. It is minted once, lazily, and shared
// by every fixture in the process: retries and reconstructed controllers inside
// one run derive the same identities, while a concurrent test process mints a
// different suffix.
var defaultScope = NewScope()

// RunScope returns this process's scope.
func RunScope() Scope { return defaultScope }

// RunScoped maps base to this process's run namespace. Call it once per fixture
// and keep the result; calling it again for the same base is safe and stable.
func RunScoped(base string) string { return defaultScope.ID(base) }

// NewScope mints an independent scope with a fresh random suffix. Concurrent
// runs (or a test that simulates two runs) must not share a scope.
func NewScope() Scope { return Scope{suffix: mintRunSuffix()} }

// ScopeWithSuffix reconstructs a scope from a known suffix. It exists so tests
// can prove the mapping deterministically; production fixtures always mint
// through NewScope/RunScope.
func ScopeWithSuffix(suffix string) Scope { return Scope{suffix: suffix} }

// Suffix is the scope's random run suffix.
func (s Scope) Suffix() string { return s.suffix }

// ID returns base suffixed with this run's namespace. The result stays a valid
// Kodama identity because the suffix is lowercase hex and the dash is allowed.
func (s Scope) ID(base string) string { return base + "-" + s.suffix }

func mintRunSuffix() string {
	var raw [runSuffixBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// Test support cannot mint an independent run namespace without
		// randomness; substituting a fixed value would silently reintroduce
		// cross-run collisions.
		panic(fmt.Sprintf("runtimesupport: mint run suffix: %v", err))
	}
	return hex.EncodeToString(raw[:])
}
