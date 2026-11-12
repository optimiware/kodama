package restorequal

// Package restorequal is the Q03 backup, restore, retention and recovery-objective
// qualification harness. It owns test code only: the pure recovery-timeline and
// stage-destination rules, the evidence allowlist, the local Podman drill and the
// stage drill skeleton.
//
// Sources:
//   - KODAMA.md "Backup and recovery" (control-plane scope) and its initial
//     objectives: RPO ≤15 minutes, RTO ≤4 hours, 30-day PostgreSQL recovery.
//   - docs/specs/10-production.md §4 (backup and restore) and §5 (retention and
//     measured targets).
//   - docs/tasks/05-qualification.md Q03.
//
// This is a pre-G03 test-code landing; the Q03 card completes only after the
// drills are re-run on the G03 revision or later.

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrInvalidTimeline reports a timeline that cannot produce an honest
// measurement.
var ErrInvalidTimeline = errors.New("invalid recovery timeline")

// Objectives are the frozen recovery targets. See KODAMA.md line 517 and
// docs/specs/10-production.md §5: RPO ≤15 minutes, RTO ≤4 hours and 30-day
// PostgreSQL recovery.
type Objectives struct {
	RPO                time.Duration
	RTO                time.Duration
	PostgreSQLRecovery time.Duration
}

// DefaultObjectives returns the frozen initial objectives.
func DefaultObjectives() Objectives {
	return Objectives{
		RPO:                15 * time.Minute,
		RTO:                4 * time.Hour,
		PostgreSQLRecovery: 30 * 24 * time.Hour,
	}
}

func (objectives Objectives) validate() error {
	if objectives.RPO <= 0 {
		return fmt.Errorf("%w: RPO must be positive", ErrInvalidTimeline)
	}
	if objectives.RTO <= 0 {
		return fmt.Errorf("%w: RTO must be positive", ErrInvalidTimeline)
	}
	if objectives.PostgreSQLRecovery <= 0 {
		return fmt.Errorf("%w: PostgreSQL recovery must be positive", ErrInvalidTimeline)
	}
	return nil
}

// Milestone names. They are the fixed vocabulary of a measured recovery
// timeline, from the backup start to the resumed service.
const (
	// MilestoneBackupStarted is when the backup run began.
	MilestoneBackupStarted = "backup.started"
	// MilestoneBackupCompleted is when the backup set was complete.
	MilestoneBackupCompleted = "backup.completed"
	// MilestoneFailureInjected is when the source became unavailable (the
	// recovery-time clock starts here).
	MilestoneFailureInjected = "failure.injected"
	// MilestoneRestoreStarted is when the restore run began.
	MilestoneRestoreStarted = "restore.started"
	// MilestoneRestoreCompleted is when durable state was restored.
	MilestoneRestoreCompleted = "restore.completed"
	// MilestoneServiceResumed is when dispatch/consumers resumed (the
	// recovery-time clock stops here).
	MilestoneServiceResumed = "service.resumed"
)

// Milestone is one timestamped event. At is always UTC.
type Milestone struct {
	Name string
	At   time.Time
}

// Watermark is the durable state observed on one side of the recovery.
// Committed is the last durable commit before the failure; Restored is the
// state actually present after the restore. RPO is measured between the two.
type Watermark struct {
	At  time.Time
	IDs []string
}

// Timeline records UTC milestones and the two durable watermarks using an
// injected clock, then measures RPO, RTO and the lost IDs.
type Timeline struct {
	clock               func() time.Time
	objectives          Objectives
	milestones          []Milestone
	committed           *Watermark
	restored            *Watermark
	postgresqlRetention *time.Duration
}

// NewTimeline builds a timeline over clock. Tests inject a deterministic clock;
// drills inject time.Now. A nil clock or a non-positive objective is refused.
func NewTimeline(clock func() time.Time, objectives Objectives) (*Timeline, error) {
	if clock == nil {
		return nil, fmt.Errorf("%w: clock is required", ErrInvalidTimeline)
	}
	if err := objectives.validate(); err != nil {
		return nil, err
	}
	if clock().IsZero() {
		return nil, fmt.Errorf("%w: clock returned the zero time", ErrInvalidTimeline)
	}
	return &Timeline{clock: clock, objectives: objectives}, nil
}

// Mark records one milestone at the injected clock, normalized to UTC. A name
// may be recorded once; duplicates and empty names are refused.
func (timeline *Timeline) Mark(name string) (Milestone, error) {
	if name == "" {
		return Milestone{}, fmt.Errorf("%w: milestone name is required", ErrInvalidTimeline)
	}
	for _, existing := range timeline.milestones {
		if existing.Name == name {
			return Milestone{}, fmt.Errorf("%w: duplicate milestone %q", ErrInvalidTimeline, name)
		}
	}
	at := timeline.clock().UTC()
	if at.IsZero() {
		return Milestone{}, fmt.Errorf("%w: clock returned the zero time for %q", ErrInvalidTimeline, name)
	}
	milestone := Milestone{Name: name, At: at}
	timeline.milestones = append(timeline.milestones, milestone)
	return milestone, nil
}

// Milestone returns one recorded milestone.
func (timeline *Timeline) Milestone(name string) (Milestone, bool) {
	for _, milestone := range timeline.milestones {
		if milestone.Name == name {
			return milestone, true
		}
	}
	return Milestone{}, false
}

// Milestones returns an independent copy of the recorded milestones.
func (timeline *Timeline) Milestones() []Milestone {
	return append([]Milestone(nil), timeline.milestones...)
}

// Commit records the last durable commit before the failure.
func (timeline *Timeline) Commit(watermark Watermark) error {
	normalized, err := normalizeWatermark(watermark)
	if err != nil {
		return err
	}
	timeline.committed = &normalized
	return nil
}

// Restore records the durable state present after the restore. The restored
// state may not be newer than the committed state, and every restored ID must
// have been committed (a restore never invents state).
func (timeline *Timeline) Restore(watermark Watermark) error {
	normalized, err := normalizeWatermark(watermark)
	if err != nil {
		return err
	}
	if timeline.committed != nil {
		if normalized.At.After(timeline.committed.At) {
			return fmt.Errorf("%w: restored watermark %v is after the committed watermark %v",
				ErrInvalidTimeline, normalized.At, timeline.committed.At)
		}
		committed := make(map[string]bool, len(timeline.committed.IDs))
		for _, id := range timeline.committed.IDs {
			committed[id] = true
		}
		for _, id := range normalized.IDs {
			if !committed[id] {
				return fmt.Errorf("%w: restored watermark contains ID %q that was never committed", ErrInvalidTimeline, id)
			}
		}
	}
	timeline.restored = &normalized
	return nil
}

// RecordPostgreSQLRetention records the observed PostgreSQL backup retention
// window so Assess evaluates Objectives.PostgreSQLRecovery.
func (timeline *Timeline) RecordPostgreSQLRetention(retention time.Duration) {
	value := retention
	timeline.postgresqlRetention = &value
}

// Assessment is one measured recovery timeline.
type Assessment struct {
	Objectives Objectives
	// RPO is the data-loss window: committed watermark minus restored
	// watermark.
	RPO time.Duration
	// RTO is the outage window: failure injected to service resumed.
	RTO time.Duration
	// PostgreSQLRetention is the observed PostgreSQL backup retention window,
	// when the drill measured one.
	PostgreSQLRetention *time.Duration
	// LostIDs are the committed IDs absent after the restore, sorted.
	LostIDs []string
	// MeetsRPO and MeetsRTO apply the frozen limit inclusively (exactly at the
	// limit still meets it).
	MeetsRPO bool
	MeetsRTO bool
	// MeetsPostgreSQLRecovery reports whether PostgreSQLRetention meets or
	// exceeds Objectives.PostgreSQLRecovery, when PostgreSQLRetention was
	// recorded.
	MeetsPostgreSQLRecovery *bool
}

// MissedObjectives names every recommended objective the measurement misses, in
// the stable order RPO, RTO, PostgreSQLRecovery. RPO, RTO and the 30-day
// PostgreSQL recovery window are recommendations (U14): a drill reports a
// missed objective as a documented limitation and never turns a completed
// restore into a failure.
func (assessment Assessment) MissedObjectives() []string {
	var missed []string
	if !assessment.MeetsRPO {
		missed = append(missed, "RPO")
	}
	if !assessment.MeetsRTO {
		missed = append(missed, "RTO")
	}
	if assessment.MeetsPostgreSQLRecovery != nil && !*assessment.MeetsPostgreSQLRecovery {
		missed = append(missed, "PostgreSQLRecovery")
	}
	return missed
}

// Assess measures the timeline. It refuses an incomplete timeline instead of
// reporting a partial objective as met.
func (timeline *Timeline) Assess() (Assessment, error) {
	if timeline.committed == nil {
		return Assessment{}, fmt.Errorf("%w: committed watermark is missing", ErrInvalidTimeline)
	}
	if timeline.restored == nil {
		return Assessment{}, fmt.Errorf("%w: restored watermark is missing", ErrInvalidTimeline)
	}
	failure, ok := timeline.Milestone(MilestoneFailureInjected)
	if !ok {
		return Assessment{}, fmt.Errorf("%w: %s milestone is missing", ErrInvalidTimeline, MilestoneFailureInjected)
	}
	if _, ok := timeline.Milestone(MilestoneRestoreCompleted); !ok {
		return Assessment{}, fmt.Errorf("%w: %s milestone is missing", ErrInvalidTimeline, MilestoneRestoreCompleted)
	}
	resumed, ok := timeline.Milestone(MilestoneServiceResumed)
	if !ok {
		return Assessment{}, fmt.Errorf("%w: %s milestone is missing", ErrInvalidTimeline, MilestoneServiceResumed)
	}
	if resumed.At.Before(failure.At) {
		return Assessment{}, fmt.Errorf("%w: service resumed %v before the failure %v",
			ErrInvalidTimeline, resumed.At, failure.At)
	}
	if timeline.restored.At.After(timeline.committed.At) {
		return Assessment{}, fmt.Errorf("%w: restored watermark is after the committed watermark", ErrInvalidTimeline)
	}
	committedIDs := make(map[string]bool, len(timeline.committed.IDs))
	for _, id := range timeline.committed.IDs {
		committedIDs[id] = true
	}
	for _, id := range timeline.restored.IDs {
		if !committedIDs[id] {
			return Assessment{}, fmt.Errorf("%w: restored watermark contains ID %q that was never committed", ErrInvalidTimeline, id)
		}
	}

	var (
		postgresqlRetention     *time.Duration
		meetsPostgreSQLRecovery *bool
	)
	if timeline.postgresqlRetention != nil {
		if *timeline.postgresqlRetention < 0 {
			return Assessment{}, fmt.Errorf("%w: PostgreSQL retention %v must not be negative", ErrInvalidTimeline, *timeline.postgresqlRetention)
		}
		retention := *timeline.postgresqlRetention
		meets := retention >= timeline.objectives.PostgreSQLRecovery
		postgresqlRetention = &retention
		meetsPostgreSQLRecovery = &meets
	}

	lost := lostIDs(timeline.committed.IDs, timeline.restored.IDs)
	rpo := timeline.committed.At.Sub(timeline.restored.At)
	rto := resumed.At.Sub(failure.At)
	return Assessment{
		Objectives:              timeline.objectives,
		RPO:                     rpo,
		RTO:                     rto,
		PostgreSQLRetention:     postgresqlRetention,
		LostIDs:                 lost,
		MeetsRPO:                rpo <= timeline.objectives.RPO,
		MeetsRTO:                rto <= timeline.objectives.RTO,
		MeetsPostgreSQLRecovery: meetsPostgreSQLRecovery,
	}, nil
}

func normalizeWatermark(watermark Watermark) (Watermark, error) {
	at := watermark.At.UTC()
	if at.IsZero() {
		return Watermark{}, fmt.Errorf("%w: watermark time is required", ErrInvalidTimeline)
	}
	seen := make(map[string]bool, len(watermark.IDs))
	ids := make([]string, 0, len(watermark.IDs))
	for _, id := range watermark.IDs {
		if id == "" {
			return Watermark{}, fmt.Errorf("%w: watermark ID is empty", ErrInvalidTimeline)
		}
		if seen[id] {
			return Watermark{}, fmt.Errorf("%w: duplicate watermark ID %q", ErrInvalidTimeline, id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return Watermark{At: at, IDs: ids}, nil
}

// lostIDs returns the committed IDs absent from the restored set, sorted.
func lostIDs(committed, restored []string) []string {
	present := make(map[string]bool, len(restored))
	for _, id := range restored {
		present[id] = true
	}
	lost := make([]string, 0, len(committed))
	for _, id := range committed {
		if !present[id] {
			lost = append(lost, id)
		}
	}
	sort.Strings(lost)
	return lost
}
