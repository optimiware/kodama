// Package parallel bounds how many infrastructure-backed tests of one test
// binary run at the same time, and serializes the ones that share a named
// external resource.
//
// A Gate is the one place a live suite turns sequential scenarios into bounded
// parallel ones. A limit of 0 or 1 keeps the package sequential, so a failing
// scenario can always be reproduced alone.
package parallel

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Gate is a counting semaphore for the parallel tests of one package.
type Gate struct {
	// Env names the environment variable that overrides Default.
	Env string
	// Default is the limit used when Env is unset.
	Default int
	// Max is the largest accepted limit. A larger, negative or malformed value
	// fails every gated test instead of silently running with another bound.
	Max int
	// HostPoolEnv optionally names the launcher's pool size for the same
	// resource (KODAMA_STACK_SLOTS or KODAMA_KIND_SLOTS). A smaller valid
	// value lowers Default, so a host sized for fewer concurrent stacks keeps
	// that bound. An explicit Env value always wins.
	HostPoolEnv string

	once  sync.Once
	slots chan struct{}
	err   error
}

// Limit returns the configured concurrency bound.
func (gate *Gate) Limit(getenv func(string) string) (int, error) {
	if raw := strings.TrimSpace(getenv(gate.Env)); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 || limit > gate.Max {
			return 0, fmt.Errorf("%s=%q: want an integer in [0, %d]", gate.Env, raw, gate.Max)
		}
		return limit, nil
	}
	limit := gate.Default
	if gate.HostPoolEnv != "" {
		if pool, err := strconv.Atoi(strings.TrimSpace(getenv(gate.HostPoolEnv))); err == nil && pool > 0 && pool < limit {
			limit = pool
		}
	}
	return limit, nil
}

func (gate *Gate) load() error {
	gate.once.Do(func() {
		limit, err := gate.Limit(os.Getenv)
		if err != nil {
			gate.err = err
			return
		}
		if limit > 1 {
			gate.slots = make(chan struct{}, limit)
		}
	})
	return gate.err
}

// Enter marks t parallel and holds one slot until t and its cleanups finish.
// Call it first, before the test starts a clock, a deadline or reads a
// credential, so time spent queued is never charged to the scenario. With a
// limit of 0 or 1 it returns immediately and t stays sequential.
func (gate *Gate) Enter(t *testing.T) {
	t.Helper()
	if err := gate.load(); err != nil {
		t.Fatal(err)
	}
	if gate.slots == nil {
		return
	}
	t.Parallel()
	gate.slots <- struct{}{}
	t.Cleanup(func() { <-gate.slots })
}

// Parallel marks t parallel without taking a slot. It is for a parent test
// that only groups subtests which Enter the gate themselves.
func (gate *Gate) Parallel(t *testing.T) {
	t.Helper()
	if err := gate.load(); err != nil {
		t.Fatal(err)
	}
	if gate.slots != nil {
		t.Parallel()
	}
}

// KeyedLock serializes tests that mutate the same named external resource
// while tests holding other keys run concurrently.
type KeyedLock struct {
	locks sync.Map
}

// Hold blocks until t owns key and releases it after t's cleanups finish.
func (lock *KeyedLock) Hold(t *testing.T, key string) {
	t.Helper()
	held, _ := lock.locks.LoadOrStore(key, &sync.Mutex{})
	mutex := held.(*sync.Mutex)
	mutex.Lock()
	t.Cleanup(mutex.Unlock)
}
