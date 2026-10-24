// Package gitwebhooktest holds the in-memory persistence adapter and the port
// doubles used by the unit acceptance tests of internal/integrations/gitwebhook
// and its HTTP adapter. The memory store proves policy (uniqueness, first-wins
// completion, optimistic versions), never SQL semantics: real persistence is
// qualified at T-038.
package gitwebhooktest

import (
	"context"
	"errors"
	"sync"
	"time"

	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
)

// Store is an in-memory gitwebhook.Store.
type Store struct {
	mu         sync.Mutex
	triggers   map[gw.TriggerID]gw.Trigger
	deliveries map[gw.DeliveryKey]gw.DeliveryRecord
	audits     []gw.AuditRecord
	// FailClaims, when set, makes ClaimDelivery fail (an outage).
	FailClaims error
	// FailFinishes makes that many FinishDelivery calls fail (lost result).
	FailFinishes int
}

var _ gw.Store = (*Store)(nil)

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{triggers: map[gw.TriggerID]gw.Trigger{}, deliveries: map[gw.DeliveryKey]gw.DeliveryRecord{}}
}

func cloneTrigger(t gw.Trigger) gw.Trigger {
	t.Branches = append([]string(nil), t.Branches...)
	return t
}

// InsertTrigger implements gitwebhook.Store.
func (s *Store) InsertTrigger(_ context.Context, t gw.Trigger, audit gw.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.triggers[t.ID]; ok {
		return gw.ErrConflict
	}
	s.triggers[t.ID] = cloneTrigger(t)
	s.audits = append(s.audits, audit)
	return nil
}

// Trigger implements gitwebhook.Store.
func (s *Store) Trigger(_ context.Context, id gw.TriggerID) (gw.Trigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.triggers[id]
	if !ok {
		return gw.Trigger{}, gw.ErrNotFound
	}
	return cloneTrigger(t), nil
}

// UpdateTrigger implements gitwebhook.Store.
func (s *Store) UpdateTrigger(_ context.Context, t gw.Trigger, expected int64, audit gw.AuditRecord) (gw.Trigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.triggers[t.ID]
	if !ok || cur.Scope != t.Scope {
		return gw.Trigger{}, gw.ErrNotFound
	}
	if cur.Version != expected {
		return gw.Trigger{}, gw.ErrConflict
	}
	t.Version = expected + 1
	s.triggers[t.ID] = cloneTrigger(t)
	s.audits = append(s.audits, audit)
	return cloneTrigger(t), nil
}

// ClaimDelivery implements gitwebhook.Store.
func (s *Store) ClaimDelivery(_ context.Context, rec gw.DeliveryRecord) (gw.DeliveryRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailClaims != nil {
		return gw.DeliveryRecord{}, false, s.FailClaims
	}
	if cur, ok := s.deliveries[rec.Key]; ok {
		return cur, false, nil
	}
	s.deliveries[rec.Key] = rec
	return rec, true, nil
}

// BeginAdmission implements gitwebhook.Store.
func (s *Store) BeginAdmission(_ context.Context, key gw.DeliveryKey) (gw.DeliveryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.deliveries[key]
	if !ok {
		return gw.DeliveryRecord{}, gw.ErrNotFound
	}
	if !cur.Done {
		cur.Admitting = true
		s.deliveries[key] = cur
	}
	return cur, nil
}

// AcquireAdmission implements gitwebhook.Store.
func (s *Store) AcquireAdmission(_ context.Context, key gw.DeliveryKey, holder string, now time.Time, ttl time.Duration) (gw.DeliveryRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.deliveries[key]
	if !ok {
		return gw.DeliveryRecord{}, false, gw.ErrNotFound
	}
	if cur.Done {
		return cur, false, nil
	}
	if !cur.Admitting {
		return gw.DeliveryRecord{}, false, gw.ErrConflict
	}
	if cur.LeaseHolder != "" && cur.LeaseHolder != holder && now.Before(cur.LeaseUntil) {
		return cur, false, nil
	}
	cur.LeaseHolder, cur.LeaseUntil = holder, now.Add(ttl)
	s.deliveries[key] = cur
	return cur, true, nil
}

// ReleaseAdmission implements gitwebhook.Store.
func (s *Store) ReleaseAdmission(_ context.Context, key gw.DeliveryKey, holder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.deliveries[key]; ok && cur.LeaseHolder == holder {
		cur.LeaseHolder, cur.LeaseUntil = "", time.Time{}
		s.deliveries[key] = cur
	}
	return nil
}

// FinishDelivery implements gitwebhook.Store; the first finish wins and the
// admission fence applies.
func (s *Store) FinishDelivery(_ context.Context, key gw.DeliveryKey, fin gw.Finish, audit gw.AuditRecord) (gw.DeliveryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailFinishes > 0 {
		s.FailFinishes--
		return gw.DeliveryRecord{}, errors.New("gitwebhooktest: injected finish failure")
	}
	cur, ok := s.deliveries[key]
	if !ok {
		return gw.DeliveryRecord{}, gw.ErrNotFound
	}
	if cur.Done {
		return cur, nil
	}
	if cur.Admitting != fin.Admission {
		return gw.DeliveryRecord{}, gw.ErrConflict
	}
	if fin.Admission && fin.Outcome != gw.OutcomeStarted && (fin.Holder == "" || cur.LeaseHolder != fin.Holder) {
		return gw.DeliveryRecord{}, gw.ErrConflict
	}
	cur.LeaseHolder, cur.LeaseUntil = "", time.Time{}
	cur.Done, cur.Outcome, cur.Reason, cur.RunID, cur.CompletedAt = true, fin.Outcome, fin.Reason, fin.RunID, audit.At
	s.deliveries[key] = cur
	s.audits = append(s.audits, audit)
	return cur, nil
}

// RecordAudit implements gitwebhook.Store.
func (s *Store) RecordAudit(_ context.Context, audit gw.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, audit)
	return nil
}

// Audits returns a copy of the audit trail.
func (s *Store) Audits() []gw.AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gw.AuditRecord(nil), s.audits...)
}

// Delivery returns a stored delivery record.
func (s *Store) Delivery(key gw.DeliveryKey) (gw.DeliveryRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.deliveries[key]
	return r, ok
}

// DeliveryCount is the number of claimed deliveries.
func (s *Store) DeliveryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deliveries)
}
