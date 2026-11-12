package restorequal

// Q03 timeline tests. They pin the recovery objectives from KODAMA.md lines
// 515-517 and docs/specs/10-production.md §5: RPO ≤15 minutes, RTO ≤4 hours and
// 30-day PostgreSQL recovery. The timeline builder takes an injected clock so
// the exact and just-past boundaries are deterministic.

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var timelineEpoch = time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)

// TestRecoveryTimeline covers the pure timeline builder the Q03 drills use to
// record a timestamped recovery timeline.
func TestRecoveryTimeline(t *testing.T) {
	t.Run("milestones are UTC and unique", func(t *testing.T) {
		zone := time.FixedZone("CET", 60*60)
		wall := time.Date(2026, 11, 8, 10, 0, 0, 0, zone)
		timeline, err := NewTimeline(func() time.Time { return wall }, DefaultObjectives())
		if err != nil {
			t.Fatalf("NewTimeline: %v", err)
		}
		first, err := timeline.Mark(MilestoneBackupStarted)
		if err != nil {
			t.Fatalf("Mark(backup.started): %v", err)
		}
		want := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
		if !first.At.Equal(want) {
			t.Fatalf("milestone time = %v; want %v", first.At, want)
		}
		if first.At.Location() != time.UTC {
			t.Fatalf("milestone location = %v; want UTC", first.At.Location())
		}
		if _, err := timeline.Mark(MilestoneBackupStarted); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("duplicate milestone error = % v; want ErrInvalidTimeline", err)
		}
		if _, err := timeline.Mark(""); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("empty milestone error = %v; want ErrInvalidTimeline", err)
		}
		if _, ok := timeline.Milestone(MilestoneBackupStarted); !ok {
			t.Fatal("recorded milestone is not readable")
		}
		if _, ok := timeline.Milestone("missing"); ok {
			t.Fatal("unrecorded milestone reported present")
		}
		recorded := timeline.Milestones()
		recorded[0].Name = "mutated"
		if got := timeline.Milestones()[0].Name; got != MilestoneBackupStarted {
			t.Fatalf("milestones are not copied; first name = %q", got)
		}
	})

	t.Run("builder refuses a missing clock and invalid objectives", func(t *testing.T) {
		if _, err := NewTimeline(nil, DefaultObjectives()); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("nil clock error = %v; want ErrInvalidTimeline", err)
		}
		if _, err := NewTimeline(func() time.Time { return timelineEpoch }, Objectives{}); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("zero objectives error = %v; want ErrInvalidTimeline", err)
		}
		if _, err := NewTimeline(func() time.Time { return time.Time{} }, DefaultObjectives()); err == nil {
			t.Fatal("zero clock time was accepted")
		}
	})

	t.Run("objectives match the frozen targets", func(t *testing.T) {
		objectives := DefaultObjectives()
		if objectives.RPO != 15*time.Minute {
			t.Fatalf("RPO = %v; want 15m", objectives.RPO)
		}
		if objectives.RTO != 4*time.Hour {
			t.Fatalf("RTO = %v; want 4h", objectives.RTO)
		}
		if objectives.PostgreSQLRecovery != 30*24*time.Hour {
			t.Fatalf("PostgreSQL recovery = %v; want 30 days", objectives.PostgreSQLRecovery)
		}
	})

	t.Run("RPO and RTO boundaries are exact", func(t *testing.T) {
		cases := []struct {
			name     string
			rpo      time.Duration
			rto      time.Duration
			meetsRPO bool
			meetsRTO bool
		}{
			{name: "well within", rpo: 0, rto: 30 * time.Minute, meetsRPO: true, meetsRTO: true},
			{name: "RPO exactly at the limit", rpo: 15 * time.Minute, rto: time.Minute, meetsRPO: true, meetsRTO: true},
			{name: "RPO one nanosecond past", rpo: 15*time.Minute + time.Nanosecond, rto: time.Minute, meetsRPO: false, meetsRTO: true},
			{name: "RTO exactly at the limit", rpo: time.Minute, rto: 4 * time.Hour, meetsRPO: true, meetsRTO: true},
			{name: "RTO one nanosecond past", rpo: time.Minute, rto: 4*time.Hour + time.Nanosecond, meetsRPO: true, meetsRTO: false},
			{name: "both one nanosecond past", rpo: 15*time.Minute + time.Nanosecond, rto: 4*time.Hour + time.Nanosecond, meetsRPO: false, meetsRTO: false},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				timeline := newTestTimeline(t)
				committed := timelineEpoch.Add(30 * time.Minute)
				timeline.mustMark(t, MilestoneFailureInjected, committed)
				timeline.mustMark(t, MilestoneRestoreCompleted, committed.Add(time.Second))
				timeline.mustMark(t, MilestoneServiceResumed, committed.Add(testCase.rto))
				if err := timeline.Commit(Watermark{At: committed, IDs: []string{"receipt-1"}}); err != nil {
					t.Fatalf("Commit: %v", err)
				}
				if err := timeline.Restore(Watermark{At: committed.Add(-testCase.rpo), IDs: []string{"receipt-1"}}); err != nil {
					t.Fatalf("Restore: %v", err)
				}
				assessment, err := timeline.Assess()
				if err != nil {
					t.Fatalf("Assess: %v", err)
				}
				if assessment.RPO != testCase.rpo {
					t.Fatalf("RPO = %v; want %v", assessment.RPO, testCase.rpo)
				}
				if assessment.RTO != testCase.rto {
					t.Fatalf("RTO = %v; want %v", assessment.RTO, testCase.rto)
				}
				if assessment.MeetsRPO != testCase.meetsRPO {
					t.Fatalf("MeetsRPO = %v; want %v", assessment.MeetsRPO, testCase.meetsRPO)
				}
				if assessment.MeetsRTO != testCase.meetsRTO {
					t.Fatalf("MeetsRTO = %v; want %v", assessment.MeetsRTO, testCase.meetsRTO)
				}
			})
		}
	})

	t.Run("lost IDs are the committed IDs absent from the restore", func(t *testing.T) {
		timeline := newTestTimeline(t)
		committedAt := timelineEpoch.Add(time.Hour)
		timeline.mustMark(t, MilestoneFailureInjected, committedAt)
		timeline.mustMark(t, MilestoneRestoreCompleted, committedAt.Add(5*time.Minute))
		timeline.mustMark(t, MilestoneServiceResumed, committedAt.Add(10*time.Minute))
		if err := timeline.Commit(Watermark{
			At:  committedAt,
			IDs: []string{"queued-1", "receipt-1", "binding-1", "decision-1"},
		}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := timeline.Restore(Watermark{
			At:  committedAt.Add(-5 * time.Minute),
			IDs: []string{"binding-1", "receipt-1"},
		}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		assessment, err := timeline.Assess()
		if err != nil {
			t.Fatalf("Assess: %v", err)
		}
		if want := []string{"decision-1", "queued-1"}; !reflect.DeepEqual(assessment.LostIDs, want) {
			t.Fatalf("LostIDs = %v; want %v", assessment.LostIDs, want)
		}
		if assessment.RPO != 5*time.Minute {
			t.Fatalf("RPO = %v; want 5m", assessment.RPO)
		}
	})

	t.Run("inconsistent watermarks are refused", func(t *testing.T) {
		base := func(t *testing.T) *testTimeline {
			timeline := newTestTimeline(t)
			timeline.mustMark(t, MilestoneFailureInjected, timelineEpoch)
			timeline.mustMark(t, MilestoneRestoreCompleted, timelineEpoch.Add(30*time.Second))
			timeline.mustMark(t, MilestoneServiceResumed, timelineEpoch.Add(time.Minute))
			return timeline
		}

		timeline := base(t)
		if err := timeline.Commit(Watermark{At: timelineEpoch, IDs: []string{"a", "a"}}); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("duplicate committed ID error = %v; want ErrInvalidTimeline", err)
		}

		timeline = base(t)
		if err := timeline.Commit(Watermark{At: timelineEpoch, IDs: []string{"a"}}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := timeline.Restore(Watermark{At: timelineEpoch.Add(time.Minute), IDs: []string{"a"}}); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("restored-after-committed error = %v; want ErrInvalidTimeline", err)
		}

		timeline = base(t)
		if err := timeline.Commit(Watermark{At: timelineEpoch}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := timeline.Restore(Watermark{At: timelineEpoch, IDs: []string{"never-committed"}}); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("restored-only ID error = %v; want ErrInvalidTimeline", err)
		}

		timeline = base(t)
		if err := timeline.Commit(Watermark{}); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("zero committed watermark error = %v; want ErrInvalidTimeline", err)
		}

		// Restore may be recorded before commit; assessment still refuses a
		// restored ID that was never committed.
		timeline = base(t)
		if err := timeline.Restore(Watermark{At: timelineEpoch, IDs: []string{"restored-only"}}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if err := timeline.Commit(Watermark{At: timelineEpoch, IDs: []string{"committed-only"}}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if _, err := timeline.Assess(); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("assessment accepted a restored-only ID: %v", err)
		}
	})

	t.Run("assessment refuses an unfinished timeline", func(t *testing.T) {
		timeline := newTestTimeline(t)
		if _, err := timeline.Assess(); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("empty timeline assessment error = %v; want ErrInvalidTimeline", err)
		}

		cases := []struct {
			name       string
			milestones []struct {
				name string
				at   time.Time
			}
			commit  bool
			restore bool
		}{
			{
				name: "missing restore watermark",
				milestones: []struct {
					name string
					at   time.Time
				}{
					{MilestoneFailureInjected, timelineEpoch},
					{MilestoneRestoreCompleted, timelineEpoch.Add(30 * time.Second)},
					{MilestoneServiceResumed, timelineEpoch.Add(time.Minute)},
				},
				commit: true,
			},
			{
				name: "missing restore.completed",
				milestones: []struct {
					name string
					at   time.Time
				}{
					{MilestoneFailureInjected, timelineEpoch},
					{MilestoneServiceResumed, timelineEpoch.Add(time.Minute)},
				},
				commit:  true,
				restore: true,
			},
			{
				name: "missing service.resumed",
				milestones: []struct {
					name string
					at   time.Time
				}{
					{MilestoneFailureInjected, timelineEpoch},
					{MilestoneRestoreCompleted, timelineEpoch.Add(30 * time.Second)},
				},
				commit:  true,
				restore: true,
			},
			{
				name: "service resumes before the failure",
				milestones: []struct {
					name string
					at   time.Time
				}{
					{MilestoneFailureInjected, timelineEpoch.Add(time.Minute)},
					{MilestoneRestoreCompleted, timelineEpoch.Add(30 * time.Second)},
					{MilestoneServiceResumed, timelineEpoch},
				},
				commit:  true,
				restore: true,
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				timeline := newTestTimeline(t)
				for _, milestone := range testCase.milestones {
					timeline.mustMark(t, milestone.name, milestone.at)
				}
				if testCase.commit {
					if err := timeline.Commit(Watermark{At: timelineEpoch}); err != nil {
						t.Fatalf("Commit: %v", err)
					}
				}
				if testCase.restore {
					if err := timeline.Restore(Watermark{At: timelineEpoch}); err != nil {
						t.Fatalf("Restore: %v", err)
					}
				}
				if _, err := timeline.Assess(); !errors.Is(err, ErrInvalidTimeline) {
					t.Fatalf("assessment error = %v; want ErrInvalidTimeline", err)
				}
			})
		}
	})
}

// testTimeline is a Timeline with a mutable injected clock.
type testTimeline struct {
	*Timeline
	now time.Time
}

func newTestTimeline(t *testing.T) *testTimeline {
	t.Helper()
	holder := &testTimeline{now: timelineEpoch}
	timeline, err := NewTimeline(func() time.Time { return holder.now }, DefaultObjectives())
	if err != nil {
		t.Fatalf("NewTimeline: %v", err)
	}
	holder.Timeline = timeline
	return holder
}

func (timeline *testTimeline) mustMark(t *testing.T, name string, at time.Time) {
	t.Helper()
	timeline.now = at
	milestone, err := timeline.Mark(name)
	if err != nil {
		t.Fatalf("Mark(%s): %v", name, err)
	}
	if !milestone.At.Equal(at.UTC()) {
		t.Fatalf("Mark(%s).At = %v; want %v", name, milestone.At, at.UTC())
	}
}

// TestTimelineEvidenceAllowlist pins the evidence shape the drills write: the
// allowlisted keys only, with durations in milliseconds and IDs in sorted order.
func TestTimelineEvidenceAllowlist(t *testing.T) {
	timeline := newTestTimeline(t)
	timeline.mustMark(t, MilestoneBackupStarted, timelineEpoch)
	timeline.mustMark(t, MilestoneBackupCompleted, timelineEpoch.Add(time.Minute))
	timeline.mustMark(t, MilestoneFailureInjected, timelineEpoch.Add(2*time.Minute))
	timeline.mustMark(t, MilestoneRestoreStarted, timelineEpoch.Add(3*time.Minute))
	timeline.mustMark(t, MilestoneRestoreCompleted, timelineEpoch.Add(5*time.Minute))
	timeline.mustMark(t, MilestoneServiceResumed, timelineEpoch.Add(6*time.Minute))
	if err := timeline.Commit(Watermark{At: timelineEpoch.Add(2 * time.Minute), IDs: []string{"receipt-1", "decision-1"}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := timeline.Restore(Watermark{At: timelineEpoch.Add(time.Minute), IDs: []string{"receipt-1"}}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	evidence, err := TimelineEvidence("local-restore", "run-abc123", "local-podman", "confirmed", RPOBaseBackupAge, timeline.Timeline)
	if err != nil {
		t.Fatalf("TimelineEvidence: %v", err)
	}
	if evidence.RPOMilliseconds != int64(time.Minute/time.Millisecond) {
		t.Fatalf("RPO ms = %d; want %d", evidence.RPOMilliseconds, time.Minute/time.Millisecond)
	}
	if evidence.RTOMilliseconds != int64(4*time.Minute/time.Millisecond) {
		t.Fatalf("RTO ms = %d; want %d", evidence.RTOMilliseconds, 4*time.Minute/time.Millisecond)
	}
	if !reflect.DeepEqual(evidence.LostIDs, []string{"decision-1"}) {
		t.Fatalf("lost IDs = %v; want [decision-1]", evidence.LostIDs)
	}
	if len(evidence.Milestones) != 6 {
		t.Fatalf("milestones = %d; want 6", len(evidence.Milestones))
	}
	if evidence.Milestones[0].At.Location() != time.UTC {
		t.Fatalf("evidence milestone location = %v; want UTC", evidence.Milestones[0].At.Location())
	}
	if evidence.Outcome != "confirmed" || evidence.CommittedWatermark == nil || evidence.RestoredWatermark == nil {
		t.Fatalf("evidence = %+v; want outcome and both watermarks", evidence)
	}
	if evidence.RPOBase != RPOBaseBackupAge {
		t.Fatalf("rpo basis = %q; want %q", evidence.RPOBase, RPOBaseBackupAge)
	}
	if evidence.MeetsRPO == nil || !*evidence.MeetsRPO {
		t.Fatalf("backup-age evidence meets_rpo = %v; want true", evidence.MeetsRPO)
	}
	if !strings.Contains(evidence.Drill, "local-restore") {
		t.Fatalf("drill = %q", evidence.Drill)
	}
}

// TestAssessmentMissedObjectives pins the U14 reporting rule: RPO, RTO and the
// 30-day PostgreSQL recovery window are recommendations. The assessment names
// every objective it missed so a drill reports the limitation without failing a
// successful restore.
func TestAssessmentMissedObjectives(t *testing.T) {
	boolPtr := func(value bool) *bool { return &value }
	cases := []struct {
		name     string
		rpo      time.Duration
		rto      time.Duration
		meetsPGR *bool
		missed   []string
	}{
		{name: "all met", rpo: time.Minute, rto: time.Hour},
		{name: "all met with 30d retention", rpo: time.Minute, rto: time.Hour, meetsPGR: boolPtr(true)},
		{name: "RTO missed", rpo: time.Minute, rto: 4*time.Hour + time.Second, missed: []string{"RTO"}},
		{name: "RPO missed", rpo: 15*time.Minute + time.Second, rto: time.Hour, missed: []string{"RPO"}},
		{name: "PostgreSQLRecovery missed", rpo: time.Minute, rto: time.Hour, meetsPGR: boolPtr(false), missed: []string{"PostgreSQLRecovery"}},
		{name: "all three missed", rpo: 16 * time.Minute, rto: 5 * time.Hour, meetsPGR: boolPtr(false), missed: []string{"RPO", "RTO", "PostgreSQLRecovery"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assessment := Assessment{
				Objectives:              DefaultObjectives(),
				RPO:                     testCase.rpo,
				RTO:                     testCase.rto,
				MeetsRPO:                testCase.rpo <= DefaultObjectives().RPO,
				MeetsRTO:                testCase.rto <= DefaultObjectives().RTO,
				MeetsPostgreSQLRecovery: testCase.meetsPGR,
			}
			if got := assessment.MissedObjectives(); !reflect.DeepEqual(got, testCase.missed) {
				t.Fatalf("MissedObjectives = %v; want %v", got, testCase.missed)
			}
		})
	}
}

func TestRecordPostgreSQLRetention(t *testing.T) {
	base := func(t *testing.T) *testTimeline {
		t.Helper()
		timeline := newTestTimeline(t)
		timeline.mustMark(t, MilestoneFailureInjected, timelineEpoch)
		timeline.mustMark(t, MilestoneRestoreCompleted, timelineEpoch.Add(time.Minute))
		timeline.mustMark(t, MilestoneServiceResumed, timelineEpoch.Add(2*time.Minute))
		if err := timeline.Commit(Watermark{At: timelineEpoch, IDs: []string{"receipt-1"}}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := timeline.Restore(Watermark{At: timelineEpoch, IDs: []string{"receipt-1"}}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		return timeline
	}

	t.Run("exact 30-day boundary meets the objective", func(t *testing.T) {
		timeline := base(t)
		timeline.RecordPostgreSQLRetention(30 * 24 * time.Hour)
		assessment, err := timeline.Assess()
		if err != nil {
			t.Fatalf("Assess: %v", err)
		}
		if assessment.PostgreSQLRetention == nil || *assessment.PostgreSQLRetention != 30*24*time.Hour {
			t.Fatalf("PostgreSQLRetention = %v; want 720h", assessment.PostgreSQLRetention)
		}
		if assessment.MeetsPostgreSQLRecovery == nil || !*assessment.MeetsPostgreSQLRecovery {
			t.Fatalf("MeetsPostgreSQLRecovery = %v; want true", assessment.MeetsPostgreSQLRecovery)
		}
		if missed := assessment.MissedObjectives(); len(missed) != 0 {
			t.Fatalf("MissedObjectives = %v; want none", missed)
		}
		evidence, err := TimelineEvidence("stage-restore", "run-abc123", "scaleway-stage", "confirmed", RPOBaseBackupAge, timeline.Timeline)
		if err != nil {
			t.Fatalf("TimelineEvidence: %v", err)
		}
		if evidence.PostgreSQLRetentionHours == nil || *evidence.PostgreSQLRetentionHours != 720 {
			t.Fatalf("PostgreSQLRetentionHours = %v; want 720", evidence.PostgreSQLRetentionHours)
		}
		if evidence.MeetsPostgreSQLRecovery == nil || !*evidence.MeetsPostgreSQLRecovery {
			t.Fatalf("MeetsPostgreSQLRecovery = %v; want true", evidence.MeetsPostgreSQLRecovery)
		}
	})

	t.Run("one nanosecond short misses the objective", func(t *testing.T) {
		timeline := base(t)
		timeline.RecordPostgreSQLRetention(30*24*time.Hour - time.Nanosecond)
		assessment, err := timeline.Assess()
		if err != nil {
			t.Fatalf("Assess: %v", err)
		}
		if assessment.MeetsPostgreSQLRecovery == nil || *assessment.MeetsPostgreSQLRecovery {
			t.Fatalf("MeetsPostgreSQLRecovery = %v; want false", assessment.MeetsPostgreSQLRecovery)
		}
		if want := []string{"PostgreSQLRecovery"}; !reflect.DeepEqual(assessment.MissedObjectives(), want) {
			t.Fatalf("MissedObjectives = %v; want %v", assessment.MissedObjectives(), want)
		}
	})

	t.Run("negative retention is refused", func(t *testing.T) {
		timeline := base(t)
		timeline.RecordPostgreSQLRetention(-time.Second)
		if _, err := timeline.Assess(); !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("Assess error = %v; want ErrInvalidTimeline", err)
		}
	})
}
