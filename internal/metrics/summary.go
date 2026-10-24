package metrics

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Status says whether a summary could be derived.
type Status string

const (
	// StatusOK: the item's recorded history is a coherent chain and every
	// figure is derived from it.
	StatusOK Status = "ok"
	// StatusNotStarted: no history was recorded before the window ended, so the
	// item had no flow in the window.
	StatusNotStarted Status = "not_started"
	// StatusInvalidHistory: the recorded history violates the chain invariants
	// (order, versions, states, timestamps). No durations are reported.
	StatusInvalidHistory Status = "invalid_history"
)

// State names a work item state in the stored vocabulary.
type State string

// Finished reports whether the state closes an unfinished episode.
func (s State) Finished() bool { return s == "done" || s == "cancelled" }

func (s State) valid() bool {
	switch s {
	case "backlog", "ready", "active", "review", "done", "cancelled":
		return true
	}
	return false
}

// WorkSummary is the flow observation of one work item over a Window. It is a
// read model derived from immutable history, not an aggregate: nothing mutates
// it and nothing persists it.
//
// Durations are clipped to the window. Blocked time overlaps Active and Review
// time and is not additive with them. Open intervals are capped at Window.To;
// the cap is reported through OpenAtEnd and is not a completion.
type WorkSummary struct {
	Scope  domain.Scope
	ItemID domain.WorkItemID
	Window Window
	Status Status
	// Anomaly names the first chain violation when Status is invalid_history.
	Anomaly string
	// ObservedVersion is the newest history version recorded before Window.To
	// (0 when none): the revision of history the figures were derived from.
	ObservedVersion domain.Version

	Lead     time.Duration // unfinished time: backlog, ready, active and review
	Active   time.Duration
	Review   time.Duration
	Blocked  time.Duration // unfinished time with a recorded blocked reason
	Reopened time.Duration // lead time of episodes begun by reopening

	// ReopenCount counts done -> active transitions recorded inside the window.
	ReopenCount int
	// BlockedIntervals counts blocked runs with a non-empty overlap with the window.
	BlockedIntervals int
	// CompletedEpisodes and CancelledEpisodes count unfinished episodes that
	// ended inside the window, by outcome.
	CompletedEpisodes int
	CancelledEpisodes int
	// CompletedLead sums the whole (unclipped) duration of episodes that ended
	// done inside the window.
	CompletedLead time.Duration

	// InFlightAtStart: the item was in an unfinished state when the window opened
	// and had entered it earlier, so Lead and the others exclude earlier time.
	InFlightAtStart bool
	// OpenAtEnd: the item was still unfinished when the window closed; its last
	// interval is capped at Window.To.
	OpenAtEnd bool
	// StateAtEnd is the state just before Window.To; empty when NotStarted.
	StateAtEnd State
}

// Validate checks the internal coherence of a value returned by an adapter.
func (s WorkSummary) Validate() error {
	if err := s.Scope.Validate(); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	if err := s.ItemID.Validate(); err != nil {
		return fmt.Errorf("item id: %w", err)
	}
	if !s.Window.Valid() {
		return fmt.Errorf("window is invalid")
	}
	for name, d := range map[string]time.Duration{
		"lead": s.Lead, "active": s.Active, "review": s.Review, "blocked": s.Blocked,
		"reopened": s.Reopened, "completed lead": s.CompletedLead,
	} {
		if d < 0 {
			return fmt.Errorf("%s duration is negative", name)
		}
	}
	for name, n := range map[string]int{
		"reopen count": s.ReopenCount, "blocked intervals": s.BlockedIntervals,
		"completed episodes": s.CompletedEpisodes, "cancelled episodes": s.CancelledEpisodes,
	} {
		if n < 0 {
			return fmt.Errorf("%s is negative", name)
		}
	}
	if s.ObservedVersion < 0 {
		return fmt.Errorf("observed version is negative")
	}
	span := s.Window.Duration()
	for name, d := range map[string]time.Duration{
		"lead": s.Lead, "active": s.Active, "review": s.Review, "blocked": s.Blocked, "reopened": s.Reopened,
	} {
		if d > span {
			return fmt.Errorf("%s duration exceeds the window", name)
		}
	}
	if s.Active+s.Review > s.Lead || s.Blocked > s.Lead || s.Reopened > s.Lead {
		return fmt.Errorf("a component duration exceeds lead time")
	}
	switch s.Status {
	case StatusOK:
		if s.Anomaly != "" || s.ObservedVersion < 1 || !s.StateAtEnd.valid() {
			return fmt.Errorf("an ok summary needs history and a state at the end")
		}
		if s.OpenAtEnd == s.StateAtEnd.Finished() {
			return fmt.Errorf("open-at-end disagrees with the state at the end")
		}
	case StatusNotStarted:
		if s.Anomaly != "" || s.ObservedVersion != 0 || s.StateAtEnd != "" || s.hasFigures() {
			return fmt.Errorf("a not-started summary carries no figures")
		}
	case StatusInvalidHistory:
		if s.Anomaly == "" || s.StateAtEnd != "" || s.hasFigures() || s.ObservedVersion != 0 {
			return fmt.Errorf("an invalid-history summary names its anomaly and carries no figures")
		}
	default:
		return fmt.Errorf("unknown status %q", string(s.Status))
	}
	return nil
}

func (s WorkSummary) hasFigures() bool {
	return s.Lead != 0 || s.Active != 0 || s.Review != 0 || s.Blocked != 0 || s.Reopened != 0 ||
		s.ReopenCount != 0 || s.BlockedIntervals != 0 || s.CompletedEpisodes != 0 ||
		s.CancelledEpisodes != 0 || s.CompletedLead != 0 || s.InFlightAtStart || s.OpenAtEnd
}
