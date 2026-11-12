// Package identity provides in-process deterministic fixtures for the
// identity and public-boundary integration suites.
//
// It contains three clearly separated fixtures:
//
//   - Provider is an in-process OIDC identity provider served over loopback
//     TLS. It publishes a discovery document and JWKS, mints signed access
//     tokens with controllable claims and offers deterministic failure
//     injection.
//   - DiscoveryServer serves the fixed Kodama public discovery route over
//     loopback TLS with a replaceable document and failure injection.
//   - ContractServer is a fixture of the Shell verification/report facts. It
//     implements the public actor.verify and resource.report endpoints over
//     the api DTOs.
//
// ContractServer is NOT the Shell composition and nothing here claims
// full approval proof. It models the public facts the enforcing-service
// boundary consumes so the Userland client, Git verification client and
// credential helper can be exercised over a real HTTP transport without a
// factory database, broker or composition root.
package identity

import (
	"sync"
	"time"
)

// Clock is a deterministic, concurrency-safe test clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock fixed at now.
func NewClock(now time.Time) *Clock {
	return &Clock{now: now}
}

// Now returns the current fixed time.
func (clock *Clock) Now() time.Time {
	if clock == nil {
		return time.Time{}
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

// Advance moves the clock forward by duration.
func (clock *Clock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// DefaultClockTime is the deterministic instant every fixture starts from
// unless the caller supplies another clock.
func DefaultClockTime() time.Time {
	return time.Unix(1_700_000_000, 0).UTC()
}
