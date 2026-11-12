package session

import (
	"fmt"
	"time"
)

// DefaultRawContentRetention is the authorized opt-in raw-content window.
const DefaultRawContentRetention = 7 * 24 * time.Hour

// RetentionPolicy controls whether raw model/tool content is retained and for
// how long. Raw content is off by default; an authorized caller opts in with
// RawContent and receives the seven-day default unless it sets Retention.
type RetentionPolicy struct {
	RawContent            bool          `json:"raw_content,omitempty"`
	Retention             time.Duration `json:"retention,omitempty"`
	PlaceholderTranscript bool          `json:"placeholder_transcript,omitempty"`
}

// Validate refuses a negative retention window.
func (policy RetentionPolicy) Validate() error {
	if policy.Retention < 0 {
		return fmt.Errorf("retention %v is negative", policy.Retention)
	}
	return nil
}

// EffectiveRetention returns the active window: zero when raw content is off,
// the configured window when set, and the seven-day default otherwise.
func (policy RetentionPolicy) EffectiveRetention() time.Duration {
	if !policy.RawContent {
		return 0
	}
	if policy.Retention == 0 {
		return DefaultRawContentRetention
	}
	return policy.Retention
}

// ExpiresAt reports the raw-content deadline for a record created at createdAt.
// It reports false when raw content is not retained at all.
func (policy RetentionPolicy) ExpiresAt(createdAt time.Time) (time.Time, bool) {
	if !policy.RawContent {
		return time.Time{}, false
	}
	return createdAt.Add(policy.EffectiveRetention()), true
}

// RetainRaw reports whether raw content created at createdAt is still retained
// at now. An active checkpoint reference protects it past its deadline.
func (policy RetentionPolicy) RetainRaw(now, createdAt time.Time, referenced bool) bool {
	if !policy.RawContent {
		return false
	}
	if referenced {
		return true
	}
	return now.Before(createdAt.Add(policy.EffectiveRetention()))
}

// Usage is one attributable model usage record. It never carries credentials.
type Usage struct {
	PrincipalID string `json:"principal_id"`
	Model       string `json:"model"`
	Prompt      int    `json:"prompt"`
	Completion  int    `json:"completion"`
}

// Outcome is the attributable result summary of a session. The summary is raw
// content and is withheld unless raw retention allows it.
type Outcome struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
}
