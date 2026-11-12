package recoveryfix

import (
	"sync"

	"github.com/optimiware/kodama/internal/userland/git"
)

// SourceEvents is a controllable per-receive-pack push identity generator. The
// recovery suite sets one identity per push so the same push identity can be
// delivered twice and then a fresh one, which is how duplicate delivery is
// distinguished from a new push.
//
// A push performs a `git-receive-pack` advertisement GET and a receive-pack
// POST; the smart-HTTP front handler mints one identity per such service
// request. Only the POST identity reaches the hook, so a fixed current value
// makes the push identity independent of the advertisement count.
type SourceEvents struct {
	mutex    sync.Mutex
	current  string
	fallback func() (string, error)
}

// NewSourceEvents returns a generator that uses the production crypto/rand
// generator until Set is called.
func NewSourceEvents() *SourceEvents {
	return &SourceEvents{fallback: git.NewSourceEventID()}
}

// Set replaces the identity every subsequent receive-pack service request uses.
func (events *SourceEvents) Set(value string) {
	events.mutex.Lock()
	defer events.mutex.Unlock()
	events.current = value
}

// Next returns the current push identity.
func (events *SourceEvents) Next() (string, error) {
	events.mutex.Lock()
	current := events.current
	fallback := events.fallback
	events.mutex.Unlock()
	if current != "" {
		return current, nil
	}
	return fallback()
}
