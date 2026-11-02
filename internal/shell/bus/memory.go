package bus

import (
	"context"
	"errors"
	"sync"
)

// Memory is the in-process Bus of the development composition: Publish calls the
// matching consumers before it returns, so a successful Publish means every
// consumer handled the message, and a consumer's error is the publish's error
// (the outbox then retries, which is the at-least-once redelivery). It needs no
// service and holds no state across a restart: the outbox rows are the state.
type Memory struct {
	mu        sync.Mutex
	subs      []*memSub
	published []Published
	fail      error
	failures  int
}

// Published is one message accepted by the in-process bus.
type Published struct {
	Subject string
	Message Message
}

type memSub struct {
	name, filter string
	h            Handler
	stopped      bool
}

var _ Bus = (*Memory)(nil)

// NewMemory returns an empty in-process bus.
func NewMemory() *Memory { return &Memory{} }

// FailNext makes the next n Publish calls fail with err before any consumer runs
// (a broker outage), to exercise the publisher's retry.
func (m *Memory) FailNext(n int, err error) {
	m.mu.Lock()
	m.fail, m.failures = err, n
	m.mu.Unlock()
}

// Published lists the messages accepted so far, in order.
func (m *Memory) Published() []Published {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Published(nil), m.published...)
}

// Subscribe registers a consumer. One delivery goes to one consumer of a name.
func (m *Memory) Subscribe(_ context.Context, name, filter string, h Handler) (func(), error) {
	if name == "" || filter == "" || h == nil {
		return nil, errors.New("bus: a consumer needs a name, a filter and a handler")
	}
	s := &memSub{name: name, filter: filter, h: h}
	m.mu.Lock()
	m.subs = append(m.subs, s)
	m.mu.Unlock()
	return func() { m.mu.Lock(); s.stopped = true; m.mu.Unlock() }, nil
}

// Publish delivers the message to one live consumer of each matching name.
func (m *Memory) Publish(ctx context.Context, subject string, msg Message) error {
	m.mu.Lock()
	if m.failures > 0 {
		m.failures--
		err := m.fail
		m.mu.Unlock()
		return err
	}
	var targets []*memSub
	seen := map[string]bool{}
	for _, s := range m.subs {
		if !s.stopped && !seen[s.name] && MatchSubject(s.filter, subject) {
			seen[s.name] = true
			targets = append(targets, s)
		}
	}
	m.mu.Unlock()
	var first error
	for _, s := range targets {
		if err := s.h(ctx, msg); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return first
	}
	m.mu.Lock()
	m.published = append(m.published, Published{Subject: subject, Message: msg})
	m.mu.Unlock()
	return nil
}
