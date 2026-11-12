package testkit

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// Secrets is a deterministic secret-delivery fake. It records only the declared
// names of delivered secrets, never their values, so a summary can be logged or
// asserted without leaking credential material. Delivery failures are
// one-shot injections that leave no recorded names.
type Secrets struct {
	mu        sync.Mutex
	attempts  int
	failNext  error
	delivered map[id.WorkloadID][]id.SecretName
}

// NewSecrets returns an empty delivery fake.
func NewSecrets() *Secrets {
	return &Secrets{delivered: make(map[id.WorkloadID][]id.SecretName)}
}

// FailNext injects one delivery failure. A nil error is replaced by a stable
// internal failure so a test cannot accidentally inject success.
func (s *Secrets) FailNext(err error) {
	if err == nil {
		err = fmt.Errorf("testkit: injected secret delivery failure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = err
}

// Attempts returns the number of delivery attempts, including failed ones.
func (s *Secrets) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// Names returns a copy of the names delivered for one workload.
func (s *Secrets) Names(workloadID id.WorkloadID) []id.SecretName {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]id.SecretName(nil), s.delivered[workloadID]...)
}

// String summarizes the fake without rendering any secret value.
func (s *Secrets) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	workloadIDs := make([]string, 0, len(s.delivered))
	for workloadID := range s.delivered {
		workloadIDs = append(workloadIDs, string(workloadID))
	}
	sort.Strings(workloadIDs)

	var builder strings.Builder
	fmt.Fprintf(&builder, "secrets{attempts:%d", s.attempts)
	for _, rawWorkloadID := range workloadIDs {
		fmt.Fprintf(&builder, " %s:[", rawWorkloadID)
		names := s.delivered[id.WorkloadID(rawWorkloadID)]
		for index, name := range names {
			if index > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteString(string(name))
		}
		builder.WriteByte(']')
	}
	builder.WriteByte('}')
	return builder.String()
}

// Deliver records the declared names for one workload. The values are dropped
// immediately; only names are retained.
func (s *Secrets) Deliver(workloadID id.WorkloadID, values []engine.SecretValue) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.attempts++
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	names := make([]id.SecretName, len(values))
	for index, value := range values {
		names[index] = value.Name
	}
	s.delivered[workloadID] = append(s.delivered[workloadID], names...)
	return nil
}
